// Package executor ties the query language (query package) and schema storage
// (catalog package) to the B+ Tree storage engine (btree + pager packages).
//
// Execution pipeline (Phase 6):
//
//   SQL text
//     → query.Parse()          (tokenise + parse into AST)
//     → planner.Plan()         (AST + schema → physical plan tree)
//     → executor.execute()     (Volcano iterator: Project → Limit? → Filter? → IndexScan)
//     → Result
//
// Before Phase 6 the executor contained ad-hoc planning logic (planKeyRange,
// rowMatchesWhere, etc.) inlined inside execSelect.  That code is now in the
// planner package, where it can be tested independently of the storage engine.
//
// Transaction model (Phase 4)
//
//   Each INSERT is protected by a WAL transaction.  The flow for a write:
//
//     1. AllocXID — log a Begin record to the WAL
//     2. Execute the statement against a TxPager (no-steal: dirty pages buffered)
//     3. Log Write records (after-images) for every dirty page
//     4. Log Commit record + fsync WAL  ← durability point
//     5. TxPager.Flush() → BufPager.WritePage (write-through: disk + pool)
//
//   If the process crashes between steps 4 and 5, WAL replay on next Open()
//   re-applies the committed writes.  If it crashes before step 4, there is no
//   Commit record and recovery skips those writes entirely.
//
// Buffer pool (Phase 5)
//
//   DB owns a single *bufferpool.Pool shared across all open table files.
//   Table pagers stay open for the DB lifetime so the pool survives across
//   statements — repeated SELECTs on the same table hit cache instead of disk.
package executor

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/bufferpool"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/planner"
	"github.com/yahya/db-engine/query"
	"github.com/yahya/db-engine/storage"
	"github.com/yahya/db-engine/wal"
)

const (
	intColSize  = 8  // bytes per INT column
	textColSize = 48 // bytes per TEXT column; max 47 printable chars + null
)

// openFile tracks a table or secondary index file that is open for the session.
// Reusing the same name for both keeps the map management uniform.
type openTable struct {
	pg  *pager.Pager
	fid uint16
	bp  *bufferpool.BufPager
}

// DB is an open database backed by a directory.
// Each table has its own B+ Tree file (<dir>/<table>.db).
// Each secondary index has its own B+ Tree file (<dir>/<indexname>.idx).
// The schema for all tables and indexes is in <dir>/catalog.
// The WAL is in <dir>/wal.
// Both table and index pagers stay open for the lifetime of the DB so the
// buffer pool can serve repeated reads without reopening files.
type DB struct {
	dir      string
	catalog  *catalog.Catalog
	wal      *wal.WAL
	pool     *bufferpool.Pool
	openTbls map[string]*openTable // lowercase table name → open table
	openIdxs map[string]*openTable // lowercase index name → open index file
	activeTx *activeTx             // non-nil when an explicit BEGIN is in progress
}

// activeTx holds the state of an explicit transaction across multiple statements.
type activeTx struct {
	xid    uint32
	pagers map[string]*pager.TxPager // lowercase table name → TxPager
}

// Result is returned by Exec for every statement.
// For SELECT and EXPLAIN, Columns and/or Rows are populated.
// For other statements, only Message is set.
type Result struct {
	Columns []string          // column headers
	Rows    [][]catalog.Value // decoded row values
	Message string
}

// Open opens (or creates) a database at dir, runs WAL crash recovery, and
// initialises the shared buffer pool.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create database directory %q: %w", dir, err)
	}

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		return nil, fmt.Errorf("open WAL: %w", err)
	}
	if err := w.Recover(dir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("WAL recovery: %w", err)
	}

	cat, err := catalog.Load(filepath.Join(dir, "catalog"))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("load catalog: %w", err)
	}

	return &DB{
		dir:      dir,
		catalog:  cat,
		wal:      w,
		pool:     bufferpool.New(bufferpool.DefaultCapacity),
		openTbls: make(map[string]*openTable),
		openIdxs: make(map[string]*openTable),
	}, nil
}

// Close rolls back any open transaction, closes all table and index pagers, and syncs the WAL.
func (db *DB) Close() error {
	if db.activeTx != nil {
		_ = db.rollbackTx(db.activeTx)
		db.activeTx = nil
	}
	for key, ot := range db.openTbls {
		db.pool.Unregister(ot.fid)
		_ = ot.pg.Close()
		delete(db.openTbls, key)
	}
	for key, ot := range db.openIdxs {
		db.pool.Unregister(ot.fid)
		_ = ot.pg.Close()
		delete(db.openIdxs, key)
	}
	return db.wal.Close()
}

// InTransaction reports whether an explicit BEGIN is in progress.
func (db *DB) InTransaction() bool { return db.activeTx != nil }

// PoolStats returns a snapshot of buffer pool metrics.
func (db *DB) PoolStats() bufferpool.Stats { return db.pool.Stats() }

// Exec parses and executes a SQL statement.
func (db *DB) Exec(sql string) (*Result, error) {
	stmt, err := query.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	switch s := stmt.(type) {
	case *query.CreateTableStmt:
		return db.execCreate(s)
	case *query.CreateIndexStmt:
		return db.execCreateIndex(s)
	case *query.DropIndexStmt:
		return db.execDropIndex(s)
	case *query.InsertStmt:
		return db.execInsert(s)
	case *query.SelectStmt:
		return db.execSelect(s)
	case *query.ExplainStmt:
		return db.execExplain(s)
	case *query.BeginStmt:
		return db.execBegin()
	case *query.CommitStmt:
		return db.execCommit()
	case *query.RollbackStmt:
		return db.execRollback()
	default:
		return nil, fmt.Errorf("unsupported statement type %T", stmt)
	}
}

// --- transaction control ---

func (db *DB) execBegin() (*Result, error) {
	if db.activeTx != nil {
		return nil, fmt.Errorf("transaction already in progress; COMMIT or ROLLBACK first")
	}
	xid, err := db.wal.AllocXID()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	db.activeTx = &activeTx{
		xid:    xid,
		pagers: make(map[string]*pager.TxPager),
	}
	return &Result{Message: "BEGIN"}, nil
}

func (db *DB) execCommit() (*Result, error) {
	if db.activeTx == nil {
		return nil, fmt.Errorf("no active transaction")
	}
	tx := db.activeTx
	db.activeTx = nil
	if err := db.commitTx(tx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &Result{Message: "COMMIT"}, nil
}

func (db *DB) execRollback() (*Result, error) {
	if db.activeTx == nil {
		return nil, fmt.Errorf("no active transaction")
	}
	tx := db.activeTx
	db.activeTx = nil
	return &Result{Message: "ROLLBACK"}, db.rollbackTx(tx)
}

// commitTx logs all dirty pages to the WAL, fsyncs, then flushes them via BufPager
// (write-through: disk + pool update in one step).
func (db *DB) commitTx(tx *activeTx) error {
	for key, txpg := range tx.pagers {
		// Index pagers are keyed "idx:<name>" → file is "<name>.idx".
		// Table pagers are keyed "<name>" → file is "<name>.db".
		var fileName string
		if strings.HasPrefix(key, "idx:") {
			fileName = key[4:] + ".idx"
		} else {
			fileName = key + ".db"
		}
		for _, page := range txpg.DirtyPages() {
			raw, err := storage.Encode(page)
			if err != nil {
				return err
			}
			if err := db.wal.AppendWrite(tx.xid, fileName, page.Header.PageID, raw); err != nil {
				return err
			}
		}
	}
	if err := db.wal.AppendCommit(tx.xid); err != nil {
		return err
	}
	if err := db.wal.Sync(); err != nil {
		return err
	}
	for _, txpg := range tx.pagers {
		if err := txpg.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) rollbackTx(tx *activeTx) error {
	for _, txpg := range tx.pagers {
		_ = txpg.Rollback()
	}
	return db.wal.AppendRollback(tx.xid)
}

// getOrOpenTable returns the BufPager for the named table, opening the pager
// and registering it with the pool on first access.
func (db *DB) getOrOpenTable(name string) (*bufferpool.BufPager, error) {
	key := strings.ToLower(name)
	if ot, ok := db.openTbls[key]; ok {
		return ot.bp, nil
	}
	pg, err := pager.Open(db.tablePath(name))
	if err != nil {
		return nil, fmt.Errorf("open table %q: %w", name, err)
	}
	fid := db.pool.Register(pg)
	bp := bufferpool.NewBufPager(db.pool, pg, fid)
	db.openTbls[key] = &openTable{pg: pg, fid: fid, bp: bp}
	return bp, nil
}

// txPagerForTable returns the TxPager for the named table in the active
// transaction, wrapping a BufPager on first access.
func (db *DB) txPagerForTable(name string) (*pager.TxPager, error) {
	key := strings.ToLower(name)
	if txpg, ok := db.activeTx.pagers[key]; ok {
		return txpg, nil
	}
	bp, err := db.getOrOpenTable(name)
	if err != nil {
		return nil, err
	}
	txpg := pager.NewTxPager(bp)
	db.activeTx.pagers[key] = txpg
	return txpg, nil
}

// getOrOpenIndex returns the BufPager for the named index, opening and
// registering it with the pool on first access.
func (db *DB) getOrOpenIndex(indexName string) (*bufferpool.BufPager, error) {
	key := strings.ToLower(indexName)
	if ot, ok := db.openIdxs[key]; ok {
		return ot.bp, nil
	}
	pg, err := pager.Open(db.indexPath(indexName))
	if err != nil {
		return nil, fmt.Errorf("open index file %q: %w", indexName, err)
	}
	fid := db.pool.Register(pg)
	bp := bufferpool.NewBufPager(db.pool, pg, fid)
	db.openIdxs[key] = &openTable{pg: pg, fid: fid, bp: bp}
	return bp, nil
}

// txPagerForIndex returns the TxPager for the named index in the active
// transaction, wrapping a BufPager on first access.
func (db *DB) txPagerForIndex(indexName string) (*pager.TxPager, error) {
	// Use a distinct key prefix so index pagers don't collide with table pagers.
	key := "idx:" + strings.ToLower(indexName)
	if txpg, ok := db.activeTx.pagers[key]; ok {
		return txpg, nil
	}
	bp, err := db.getOrOpenIndex(indexName)
	if err != nil {
		return nil, err
	}
	txpg := pager.NewTxPager(bp)
	db.activeTx.pagers[key] = txpg
	return txpg, nil
}

// --- CREATE TABLE ---

func (db *DB) execCreate(s *query.CreateTableStmt) (*Result, error) {
	tbl := &catalog.Table{Name: s.TableName, Columns: s.Columns}
	if err := validateSchema(tbl); err != nil {
		return nil, err
	}
	pg, err := pager.Open(db.tablePath(s.TableName))
	if err != nil {
		return nil, fmt.Errorf("create table file: %w", err)
	}
	if _, err := btree.Create(pg); err != nil {
		_ = pg.Close()
		return nil, fmt.Errorf("init B+ Tree for %q: %w", s.TableName, err)
	}
	if err := pg.Close(); err != nil {
		return nil, err
	}
	if err := db.catalog.CreateTable(tbl); err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("table %q created", s.TableName)}, nil
}

// --- CREATE INDEX / DROP INDEX ---

// execCreateIndex creates the secondary index B-Tree file and registers the
// index definition in the catalog.
//
// The index is initially empty.  It is NOT back-filled with existing table
// data — only rows inserted after CREATE INDEX are indexed.  Full back-fill
// is deferred to a future phase (requires a table scan + bulk index build).
func (db *DB) execCreateIndex(s *query.CreateIndexStmt) (*Result, error) {
	// Validate and register in catalog (also checks table + column existence).
	def := catalog.IndexDef{
		Name:   s.IndexName,
		Table:  s.TableName,
		Column: s.Column,
	}
	if err := db.catalog.CreateIndex(def); err != nil {
		return nil, err
	}

	// Create the B-Tree file for the index.
	pg, err := pager.Open(db.indexPath(s.IndexName))
	if err != nil {
		return nil, fmt.Errorf("create index file: %w", err)
	}
	if _, err := btree.Create(pg); err != nil {
		_ = pg.Close()
		return nil, fmt.Errorf("init B+ Tree for index %q: %w", s.IndexName, err)
	}
	if err := pg.Close(); err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("index %q created on %s(%s)", s.IndexName, s.TableName, s.Column)}, nil
}

// execDropIndex removes the secondary index B-Tree file and its catalog entry.
func (db *DB) execDropIndex(s *query.DropIndexStmt) (*Result, error) {
	key := strings.ToLower(s.IndexName)

	// Close and evict the index file from the pool if it is open.
	if ot, ok := db.openIdxs[key]; ok {
		db.pool.Unregister(ot.fid)
		_ = ot.pg.Close()
		delete(db.openIdxs, key)
	}

	// Remove from catalog (also validates the index exists).
	if err := db.catalog.DropIndex(s.IndexName); err != nil {
		return nil, err
	}

	// Delete the index file.
	if err := os.Remove(db.indexPath(s.IndexName)); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove index file: %w", err)
	}
	return &Result{Message: fmt.Sprintf("index %q dropped", s.IndexName)}, nil
}

// --- INSERT ---

func (db *DB) execInsert(s *query.InsertStmt) (*Result, error) {
	tbl, ok := db.catalog.GetTable(s.TableName)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", s.TableName)
	}
	if len(s.Values) != len(tbl.Columns) {
		return nil, fmt.Errorf("table %q has %d columns but %d values provided",
			tbl.Name, len(tbl.Columns), len(s.Values))
	}
	for i, v := range s.Values {
		if v.Type != tbl.Columns[i].Type {
			return nil, fmt.Errorf("column %q expects %s, got %s",
				tbl.Columns[i].Name, tbl.Columns[i].Type, v.Type)
		}
	}
	pkIdx := tbl.PrimaryKeyIndex()
	if pkIdx < 0 {
		return nil, fmt.Errorf("table %q has no primary key column", tbl.Name)
	}
	key := s.Values[pkIdx].IntVal
	encoded := encodeRow(tbl, s.Values)

	if db.activeTx != nil {
		txpg, err := db.txPagerForTable(s.TableName)
		if err != nil {
			return nil, err
		}
		bt, err := btree.Open(txpg, 1)
		if err != nil {
			return nil, fmt.Errorf("open B+ Tree: %w", err)
		}
		if err := bt.Insert(key, encoded); err != nil {
			return nil, fmt.Errorf("insert: %w", err)
		}
		// Maintain secondary indexes inside the same transaction.
		if err := db.insertIntoIndexes(tbl, s.Values, key); err != nil {
			return nil, err
		}
		return &Result{Message: "1 row inserted"}, nil
	}

	return db.autoCommitInsert(tbl, s.TableName, key, encoded, s.Values)
}

func (db *DB) autoCommitInsert(tbl *catalog.Table, tableName string, key uint64, encoded [btree.ValueSize]byte, values []catalog.Value) (*Result, error) {
	xid, err := db.wal.AllocXID()
	if err != nil {
		return nil, err
	}

	bp, err := db.getOrOpenTable(tableName)
	if err != nil {
		_ = db.wal.AppendRollback(xid)
		return nil, err
	}

	txpg := pager.NewTxPager(bp)
	bt, err := btree.Open(txpg, 1)
	if err != nil {
		_ = txpg.Rollback()
		_ = db.wal.AppendRollback(xid)
		return nil, fmt.Errorf("open B+ Tree: %w", err)
	}

	if err := bt.Insert(key, encoded); err != nil {
		_ = txpg.Rollback()
		_ = db.wal.AppendRollback(xid)
		return nil, fmt.Errorf("insert: %w", err)
	}

	pagers := map[string]*pager.TxPager{strings.ToLower(tableName): txpg}

	// Maintain secondary indexes in the same transaction.
	for _, idx := range tbl.Indexes {
		colIdx := tbl.ColIndex(idx.Column)
		if colIdx < 0 || values[colIdx].Type != catalog.TypeInt {
			continue
		}
		idxKey := values[colIdx].IntVal

		idxBP, err := db.getOrOpenIndex(idx.Name)
		if err != nil {
			for _, p := range pagers {
				_ = p.Rollback()
			}
			_ = db.wal.AppendRollback(xid)
			return nil, fmt.Errorf("open index %q: %w", idx.Name, err)
		}
		idxTxpg := pager.NewTxPager(idxBP)
		idxBT, err := btree.Open(idxTxpg, 1)
		if err != nil {
			for _, p := range pagers {
				_ = p.Rollback()
			}
			_ = idxTxpg.Rollback()
			_ = db.wal.AppendRollback(xid)
			return nil, fmt.Errorf("open index B-Tree %q: %w", idx.Name, err)
		}

		// Check uniqueness: if the indexed value already exists, reject the insert.
		if _, found, _ := idxBT.Search(idxKey); found {
			for _, p := range pagers {
				_ = p.Rollback()
			}
			_ = idxTxpg.Rollback()
			_ = db.wal.AppendRollback(xid)
			return nil, fmt.Errorf("unique constraint violated: index %q already has an entry for %s=%d",
				idx.Name, idx.Column, idxKey)
		}

		// Encode the PK into the first 8 bytes of the index value slot.
		var idxVal [btree.ValueSize]byte
		binary.LittleEndian.PutUint64(idxVal[:8], key)
		if err := idxBT.Insert(idxKey, idxVal); err != nil {
			for _, p := range pagers {
				_ = p.Rollback()
			}
			_ = idxTxpg.Rollback()
			_ = db.wal.AppendRollback(xid)
			return nil, fmt.Errorf("insert into index %q: %w", idx.Name, err)
		}
		pagers["idx:"+strings.ToLower(idx.Name)] = idxTxpg
	}

	tx := &activeTx{xid: xid, pagers: pagers}
	if err := db.commitTx(tx); err != nil {
		for _, p := range pagers {
			_ = p.Rollback()
		}
		return nil, err
	}
	return &Result{Message: "1 row inserted"}, nil
}

// insertIntoIndexes updates all secondary indexes for a row being inserted
// inside an active explicit transaction.  It uses the transaction's existing
// TxPagers (via txPagerForIndex) so all changes share the same XID.
func (db *DB) insertIntoIndexes(tbl *catalog.Table, values []catalog.Value, pk uint64) error {
	for _, idx := range tbl.Indexes {
		colIdx := tbl.ColIndex(idx.Column)
		if colIdx < 0 || values[colIdx].Type != catalog.TypeInt {
			continue
		}
		idxKey := values[colIdx].IntVal

		idxTxpg, err := db.txPagerForIndex(idx.Name)
		if err != nil {
			return fmt.Errorf("open index %q in tx: %w", idx.Name, err)
		}
		idxBT, err := btree.Open(idxTxpg, 1)
		if err != nil {
			return fmt.Errorf("open index B-Tree %q: %w", idx.Name, err)
		}

		if _, found, _ := idxBT.Search(idxKey); found {
			return fmt.Errorf("unique constraint violated: index %q already has an entry for %s=%d",
				idx.Name, idx.Column, idxKey)
		}

		var idxVal [btree.ValueSize]byte
		binary.LittleEndian.PutUint64(idxVal[:8], pk)
		if err := idxBT.Insert(idxKey, idxVal); err != nil {
			return fmt.Errorf("insert into index %q: %w", idx.Name, err)
		}
	}
	return nil
}

// --- SELECT ---

// execSelect builds a physical plan via the planner package, then drives it
// with the Volcano iterator model.
func (db *DB) execSelect(s *query.SelectStmt) (*Result, error) {
	tbl, ok := db.catalog.GetTable(s.TableName)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", s.TableName)
	}

	plan, err := planner.Plan(s, tbl)
	if err != nil {
		return nil, err
	}

	ps, err := db.pageStoreFor(s.TableName)
	if err != nil {
		return nil, err
	}

	rows, err := execute(plan, ps, db, tbl)
	if err != nil {
		return nil, err
	}

	proj := plan.(*planner.Project)
	return &Result{Columns: proj.Columns, Rows: rows}, nil
}

// execExplain returns the physical plan as text without executing the query.
func (db *DB) execExplain(s *query.ExplainStmt) (*Result, error) {
	tbl, ok := db.catalog.GetTable(s.Inner.TableName)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", s.Inner.TableName)
	}
	plan, err := planner.Plan(s.Inner, tbl)
	if err != nil {
		return nil, err
	}
	return &Result{Message: planner.Explain(plan)}, nil
}

// pageStoreFor returns the correct PageStore for a table:
// the TxPager if inside an explicit transaction (for read-your-own-writes),
// otherwise the BufPager (pool-cached reads).
func (db *DB) pageStoreFor(tableName string) (pager.PageStore, error) {
	if db.activeTx != nil {
		return db.txPagerForTable(tableName)
	}
	return db.getOrOpenTable(tableName)
}

// --- row encoding / decoding ---

func encodeRow(tbl *catalog.Table, values []catalog.Value) [btree.ValueSize]byte {
	var buf [btree.ValueSize]byte
	off := 0
	for i, col := range tbl.Columns {
		switch col.Type {
		case catalog.TypeInt:
			binary.LittleEndian.PutUint64(buf[off:off+intColSize], values[i].IntVal)
			off += intColSize
		case catalog.TypeText:
			copy(buf[off:off+textColSize], values[i].TextVal)
			off += textColSize
		}
	}
	return buf
}

func decodeRow(tbl *catalog.Table, buf [btree.ValueSize]byte) []catalog.Value {
	row := make([]catalog.Value, len(tbl.Columns))
	off := 0
	for i, col := range tbl.Columns {
		switch col.Type {
		case catalog.TypeInt:
			row[i] = catalog.Value{
				Type:   catalog.TypeInt,
				IntVal: binary.LittleEndian.Uint64(buf[off : off+intColSize]),
			}
			off += intColSize
		case catalog.TypeText:
			raw := buf[off : off+textColSize]
			end := textColSize
			for end > 0 && raw[end-1] == 0 {
				end--
			}
			row[i] = catalog.Value{Type: catalog.TypeText, TextVal: string(raw[:end])}
			off += textColSize
		}
	}
	return row
}

func validateSchema(tbl *catalog.Table) error {
	if tbl.PrimaryKeyIndex() < 0 {
		return fmt.Errorf("table %q must have at least one INT column (used as primary key)", tbl.Name)
	}
	size := 0
	for _, c := range tbl.Columns {
		switch c.Type {
		case catalog.TypeInt:
			size += intColSize
		case catalog.TypeText:
			size += textColSize
		}
	}
	if size > btree.ValueSize {
		return fmt.Errorf("table %q: row size %d bytes exceeds B+ Tree value size %d",
			tbl.Name, size, btree.ValueSize)
	}
	return nil
}

func (db *DB) tablePath(name string) string {
	return filepath.Join(db.dir, strings.ToLower(name)+".db")
}

func (db *DB) indexPath(name string) string {
	return filepath.Join(db.dir, strings.ToLower(name)+".idx")
}
