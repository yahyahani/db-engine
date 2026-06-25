// Package executor ties the query language (query package) and schema storage
// (catalog package) to the B+ Tree storage engine (btree + pager packages).
//
// Execution pipeline for a SELECT:
//   SQL string → Parse() → *SelectStmt → planKeyRange() → RangeScan/Search
//               → decode rows → post-filter → project columns → Result
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
//   Explicit transactions (BEGIN / COMMIT / ROLLBACK) keep TxPagers open across
//   multiple statements and commit all dirty pages atomically at COMMIT.
//
// Buffer pool (Phase 5)
//
//   DB owns a single *bufferpool.Pool shared across all open table files.
//   Table pagers stay open for the DB lifetime so the pool survives across
//   statements — repeated SELECTs on the same table hit cache instead of disk.
//   All reads go through BufPager (pool → disk on miss).
//   All committed writes go through BufPager.WritePage (write-through: disk + pool).
package executor

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/bufferpool"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/query"
	"github.com/yahya/db-engine/storage"
	"github.com/yahya/db-engine/wal"
)

const (
	intColSize  = 8  // bytes per INT column
	textColSize = 48 // bytes per TEXT column; max 47 printable chars + null
)

// openTable tracks a table that has been opened for the duration of this DB session.
type openTable struct {
	pg  *pager.Pager
	fid uint16
	bp  *bufferpool.BufPager
}

// DB is an open database backed by a directory.
// Each table has its own B+ Tree file (<dir>/<table>.db).
// The schema for all tables is in <dir>/catalog.
// The WAL is in <dir>/wal.
// Table pagers stay open for the lifetime of the DB so the buffer pool can
// serve repeated reads without reopening files.
type DB struct {
	dir      string
	catalog  *catalog.Catalog
	wal      *wal.WAL
	pool     *bufferpool.Pool
	openTbls map[string]*openTable // lowercase table name → open table
	activeTx *activeTx             // non-nil when an explicit BEGIN is in progress
}

// activeTx holds the state of an explicit transaction across multiple statements.
// bases was removed in Phase 5: table pagers are now owned by DB.openTbls.
type activeTx struct {
	xid    uint32
	pagers map[string]*pager.TxPager // lowercase table name → TxPager
}

// Result is returned by Exec for every statement.
// For SELECT, Columns and Rows are populated.
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
	}, nil
}

// Close rolls back any open transaction, closes all table pagers, and syncs the WAL.
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
	case *query.InsertStmt:
		return db.execInsert(s)
	case *query.SelectStmt:
		return db.execSelect(s)
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
	for tableName, txpg := range tx.pagers {
		fileName := tableName + ".db"
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
	// Flush dirty pages via BufPager (write-through: disk first, then pool update).
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
// and registering it with the pool on first access.  The pager stays open for
// the DB lifetime so cached pages survive across statements.
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
// transaction, wrapping a BufPager on first access so reads hit the pool.
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

// --- CREATE TABLE ---

// execCreate is DDL: always auto-commits outside any explicit transaction.
// The btree file is created using a raw pager (not pool-backed) for simplicity;
// the pool will cache its pages on the first DML access.
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
		// Inside an explicit transaction: buffer writes via TxPager.
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
		return &Result{Message: "1 row inserted"}, nil
	}

	return db.autoCommitInsert(s.TableName, key, encoded)
}

// autoCommitInsert wraps a single INSERT in a full WAL transaction.
// The BufPager (pool) is the base for the TxPager so that committed pages
// end up in the pool after Flush, making subsequent reads pool hits.
func (db *DB) autoCommitInsert(tableName string, key uint64, encoded [btree.ValueSize]byte) (*Result, error) {
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

	tx := &activeTx{
		xid:    xid,
		pagers: map[string]*pager.TxPager{strings.ToLower(tableName): txpg},
	}
	if err := db.commitTx(tx); err != nil {
		_ = txpg.Rollback()
		return nil, err
	}
	return &Result{Message: "1 row inserted"}, nil
}

// --- SELECT ---

func (db *DB) execSelect(s *query.SelectStmt) (*Result, error) {
	tbl, ok := db.catalog.GetTable(s.TableName)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", s.TableName)
	}

	outCols, colIdxs, err := resolveColumns(tbl, s.Columns)
	if err != nil {
		return nil, err
	}

	minKey, maxKey := planKeyRange(tbl, s.Where)

	// Inside an explicit transaction: use the TxPager (read-your-own-writes).
	// Outside: use BufPager directly (pool-cached reads).
	var ps pager.PageStore
	if db.activeTx != nil {
		ps, err = db.txPagerForTable(s.TableName)
		if err != nil {
			return nil, err
		}
	} else {
		ps, err = db.getOrOpenTable(s.TableName)
		if err != nil {
			return nil, err
		}
	}

	bt, err := btree.Open(ps, 1)
	if err != nil {
		return nil, fmt.Errorf("open B+ Tree: %w", err)
	}

	entries, err := bt.RangeScan(minKey, maxKey)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	res := &Result{Columns: outCols}
	for _, e := range entries {
		row := decodeRow(tbl, e.Value)
		if s.Where != nil && !rowMatchesWhere(row, tbl, s.Where) {
			continue
		}
		projected := make([]catalog.Value, len(colIdxs))
		for i, idx := range colIdxs {
			projected[i] = row[idx]
		}
		res.Rows = append(res.Rows, projected)
	}
	return res, nil
}

// planKeyRange computes the tightest (minKey, maxKey) range implied by the
// WHERE conditions on the primary key column.
func planKeyRange(tbl *catalog.Table, where *query.WhereClause) (minKey, maxKey uint64) {
	minKey, maxKey = 0, math.MaxUint64
	if where == nil {
		return
	}
	pkIdx := tbl.PrimaryKeyIndex()
	if pkIdx < 0 {
		return
	}
	pkName := strings.ToLower(tbl.Columns[pkIdx].Name)

	for _, cond := range where.Conds {
		if strings.ToLower(cond.Column) != pkName || cond.Val.Type != catalog.TypeInt {
			continue
		}
		v := cond.Val.IntVal
		switch cond.Op {
		case query.OpEq:
			minKey = max64(minKey, v)
			maxKey = min64(maxKey, v)
		case query.OpGt:
			if v < math.MaxUint64 {
				minKey = max64(minKey, v+1)
			} else {
				minKey, maxKey = 1, 0
			}
		case query.OpGte:
			minKey = max64(minKey, v)
		case query.OpLt:
			if v > 0 {
				maxKey = min64(maxKey, v-1)
			} else {
				minKey, maxKey = 1, 0
			}
		case query.OpLte:
			maxKey = min64(maxKey, v)
		}
	}
	return
}

func rowMatchesWhere(row []catalog.Value, tbl *catalog.Table, where *query.WhereClause) bool {
	for _, cond := range where.Conds {
		idx := tbl.ColIndex(cond.Column)
		if idx < 0 {
			continue
		}
		if !matchCondition(row[idx], cond) {
			return false
		}
	}
	return true
}

func matchCondition(v catalog.Value, cond query.Condition) bool {
	c := cond.Val
	if v.Type != c.Type {
		return false
	}
	switch v.Type {
	case catalog.TypeInt:
		switch cond.Op {
		case query.OpEq:
			return v.IntVal == c.IntVal
		case query.OpGt:
			return v.IntVal > c.IntVal
		case query.OpLt:
			return v.IntVal < c.IntVal
		case query.OpGte:
			return v.IntVal >= c.IntVal
		case query.OpLte:
			return v.IntVal <= c.IntVal
		}
	case catalog.TypeText:
		switch cond.Op {
		case query.OpEq:
			return v.TextVal == c.TextVal
		case query.OpGt:
			return v.TextVal > c.TextVal
		case query.OpLt:
			return v.TextVal < c.TextVal
		case query.OpGte:
			return v.TextVal >= c.TextVal
		case query.OpLte:
			return v.TextVal <= c.TextVal
		}
	}
	return false
}

func resolveColumns(tbl *catalog.Table, cols []string) ([]string, []int, error) {
	if len(cols) == 1 && cols[0] == "*" {
		names := make([]string, len(tbl.Columns))
		idxs := make([]int, len(tbl.Columns))
		for i, c := range tbl.Columns {
			names[i] = c.Name
			idxs[i] = i
		}
		return names, idxs, nil
	}
	names := make([]string, len(cols))
	idxs := make([]int, len(cols))
	for i, col := range cols {
		idx := tbl.ColIndex(col)
		if idx < 0 {
			return nil, nil, fmt.Errorf("column %q not found in table %q", col, tbl.Name)
		}
		names[i] = tbl.Columns[idx].Name
		idxs[i] = idx
	}
	return names, idxs, nil
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

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
