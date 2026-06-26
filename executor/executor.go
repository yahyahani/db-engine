// Package executor ties the query language (query package) and schema storage
// (catalog package) to the B+ Tree storage engine (btree + pager packages).
//
// Execution pipeline:
//
//   SQL text
//     → query.Parse()          (tokenise + parse into AST)
//     → planner.Plan()         (AST + schema → physical plan tree)
//     → executor.execute()     (Volcano iterator: Project → Limit? → Filter? → IndexScan)
//     → Result
//
// Transaction model (Phase 4 + Phase 12)
//
//   Each INSERT is protected by a WAL transaction.  The flow for a write:
//
//     1. AllocXID — log a Begin record to the WAL
//     2. Execute the statement against a TxPager (no-steal: dirty pages buffered)
//     3. Log Write records (after-images) for every dirty page
//     4. Log Commit record + fsync WAL  ← durability point
//     5. TxPager.Flush() → BufPager.WritePage (write-through: disk + pool)
//     6. TxManager.MarkCommitted(xid) — make the row visible to new snapshots
//
// MVCC (Phase 12)
//
//   Every row value in the B-Tree carries an 8-byte MVCC header:
//
//     xmin uint32 — XID of the transaction that inserted this row.
//     xmax uint32 — XID of the transaction that deleted this row (0 = live).
//
//   At the start of each SELECT (or at BEGIN for explicit transactions) the
//   executor takes a Snapshot from the TxManager — an immutable set of all
//   committed XIDs at that moment.  Scan operators filter rows through
//   Snapshot.IsVisible(xmin, xmax): only committed, non-deleted rows are
//   returned to the caller.  Uncommitted inserts from concurrent transactions
//   are invisible.
//
// Concurrency model (Phase 12)
//
//   DB.mu (sync.RWMutex) guards shared mutable state:
//     - RLock: concurrent SELECT / EXPLAIN — multiple readers run in parallel.
//     - Lock:  all writes (INSERT, BEGIN, COMMIT, ROLLBACK, DDL) — serialised.
//
//   The open-table registry (openTbls / openIdxs) is a sync.Map so table
//   opening is safe even under RLock.  If two goroutines race to open the
//   same table, the loser discards its handle and uses the winner's.
//
//   Buffer pool (Phase 5)
//
//   DB owns a single *bufferpool.Pool shared across all open table files.
//   Table pagers stay open for the DB lifetime so the pool survives across
//   statements — repeated SELECTs on the same table hit cache instead of disk.
package executor

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/bufferpool"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/mvcc"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/planner"
	"github.com/yahya/db-engine/query"
	"github.com/yahya/db-engine/stats"
	"github.com/yahya/db-engine/storage"
	"github.com/yahya/db-engine/wal"
)

// goroutineID returns the current goroutine's numeric ID by parsing the first
// line of its stack trace ("goroutine N [running]:").
// This is used to implement per-goroutine transaction isolation: each goroutine
// that calls BEGIN gets its own activeTx entry in db.txns, independent of any
// transactions started by other goroutines on the same DB.
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	b := bytes.TrimPrefix(buf[:n], []byte("goroutine "))
	if idx := bytes.IndexByte(b, ' '); idx > 0 {
		id, _ := strconv.ParseInt(string(b[:idx]), 10, 64)
		return id
	}
	return 0
}

const (
	intColSize  = catalog.IntColSize  // bytes per INT column
	textColSize = catalog.TextColSize // bytes per TEXT column; max 47 chars + null
)

// userDataSize is the bytes available for user columns in a B-Tree value slot
// after reserving the MVCC header (xmin uint32 + xmax uint32 = 8 bytes).
const userDataSize = btree.ValueSize - mvcc.HeaderSize

// openTable tracks a table or secondary index file open for the session.
type openTable struct {
	pg  *pager.Pager
	fid uint16
	bp  *bufferpool.BufPager
}

// DB is an open database backed by a directory.
//
// Goroutine safety: multiple goroutines may call DB.Exec concurrently.
// Reads (SELECT, EXPLAIN) run in parallel under db.mu.RLock.
// Writes (INSERT, DDL, transaction control) serialise under db.mu.Lock.
//
// Each goroutine that issues BEGIN gets its own independent transaction stored
// in db.txns (keyed by goroutine ID). Multiple goroutines can each have their
// own in-flight explicit transaction simultaneously; they do not interfere.
type DB struct {
	dir     string
	catalog *catalog.Catalog
	statsDB *stats.StatsDB
	wal     *wal.WAL
	pool    *bufferpool.Pool
	txMgr   *mvcc.TxManager

	mu       sync.RWMutex // RLock: concurrent reads; Lock: exclusive writes
	openTbls sync.Map     // lowercase table name → *openTable
	openIdxs sync.Map     // lowercase index name → *openTable

	txns sync.Map // goroutine ID (int64) → *activeTx
}

// activeTx holds state of an explicit transaction across multiple statements.
type activeTx struct {
	xid    uint32
	snap   mvcc.Snapshot             // snapshot taken at BEGIN; used for all reads
	pagers map[string]*pager.TxPager // lowercase table/index key → TxPager
}

// goroutineTx returns the activeTx for the current goroutine, or nil.
func (db *DB) goroutineTx() *activeTx {
	if v, ok := db.txns.Load(goroutineID()); ok {
		return v.(*activeTx)
	}
	return nil
}

// setGoroutineTx stores tx as the current goroutine's active transaction.
func (db *DB) setGoroutineTx(tx *activeTx) {
	db.txns.Store(goroutineID(), tx)
}

// clearGoroutineTx removes the current goroutine's active transaction.
func (db *DB) clearGoroutineTx() {
	db.txns.Delete(goroutineID())
}

// Result is returned by Exec for every statement.
type Result struct {
	Columns []string
	Rows    [][]catalog.Value
	Message string
}

// Open opens (or creates) a database at dir, runs WAL crash recovery, and
// initialises the shared buffer pool and MVCC transaction manager.
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

	sdb, err := stats.LoadStatsDB(filepath.Join(dir, "stats"))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("load stats: %w", err)
	}

	// Restore MVCC visibility state: any XID that was committed before the
	// previous shutdown (or crash) must be visible to new snapshots so that
	// rows inserted by those transactions are readable after restart.
	txMgr := mvcc.New()
	committedXIDs, err := w.CommittedXIDs()
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("restore committed XIDs: %w", err)
	}
	for _, xid := range committedXIDs {
		txMgr.MarkCommitted(xid)
	}

	return &DB{
		dir:     dir,
		catalog: cat,
		statsDB: sdb,
		wal:     w,
		pool:    bufferpool.New(bufferpool.DefaultCapacity),
		txMgr:   txMgr,
	}, nil
}

// Close rolls back any open transaction for the current goroutine, closes all
// table and index pagers, and syncs the WAL.
func (db *DB) Close() error {
	if tx := db.goroutineTx(); tx != nil {
		_ = db.rollbackTx(tx)
		db.clearGoroutineTx()
	}
	db.openTbls.Range(func(k, v interface{}) bool {
		ot := v.(*openTable)
		db.pool.Unregister(ot.fid)
		_ = ot.pg.Close()
		db.openTbls.Delete(k)
		return true
	})
	db.openIdxs.Range(func(k, v interface{}) bool {
		ot := v.(*openTable)
		db.pool.Unregister(ot.fid)
		_ = ot.pg.Close()
		db.openIdxs.Delete(k)
		return true
	})
	return db.wal.Close()
}

// InTransaction reports whether the current goroutine has an active BEGIN.
func (db *DB) InTransaction() bool {
	return db.goroutineTx() != nil
}

// PoolStats returns a snapshot of buffer pool metrics.
func (db *DB) PoolStats() bufferpool.Stats { return db.pool.Stats() }

// Exec parses and executes a SQL statement.
// It is safe to call from multiple goroutines concurrently.
func (db *DB) Exec(sql string) (*Result, error) {
	stmt, err := query.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	switch s := stmt.(type) {
	case *query.SelectStmt:
		return db.execSelect(s)
	case *query.ExplainStmt:
		return db.execExplain(s)
	case *query.BeginStmt:
		db.mu.Lock()
		defer db.mu.Unlock()
		return db.execBegin()
	case *query.CommitStmt:
		db.mu.Lock()
		defer db.mu.Unlock()
		return db.execCommit()
	case *query.RollbackStmt:
		db.mu.Lock()
		defer db.mu.Unlock()
		return db.execRollback()
	case *query.InsertStmt:
		return db.execInsert(s)
	case *query.DeleteStmt:
		return db.execDelete(s)
	case *query.UpdateStmt:
		return db.execUpdate(s)
	case *query.CreateTableStmt:
		db.mu.Lock()
		defer db.mu.Unlock()
		return db.execCreate(s)
	case *query.CreateIndexStmt:
		db.mu.Lock()
		defer db.mu.Unlock()
		return db.execCreateIndex(s)
	case *query.DropIndexStmt:
		db.mu.Lock()
		defer db.mu.Unlock()
		return db.execDropIndex(s)
	case *query.AnalyzeStmt:
		db.mu.Lock()
		defer db.mu.Unlock()
		return db.execAnalyze(s)
	default:
		return nil, fmt.Errorf("unsupported statement type %T", stmt)
	}
}

// --- transaction control ---

// execBegin must be called with db.mu.Lock() held.
func (db *DB) execBegin() (*Result, error) {
	if db.goroutineTx() != nil {
		return nil, fmt.Errorf("transaction already in progress; COMMIT or ROLLBACK first")
	}
	xid, err := db.wal.AllocXID()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	db.setGoroutineTx(&activeTx{
		xid:    xid,
		snap:   db.txMgr.TakeSnapshot(xid), // snapshot of committed XIDs at BEGIN time
		pagers: make(map[string]*pager.TxPager),
	})
	return &Result{Message: "BEGIN"}, nil
}

// execCommit must be called with db.mu.Lock() held.
func (db *DB) execCommit() (*Result, error) {
	tx := db.goroutineTx()
	if tx == nil {
		return nil, fmt.Errorf("no active transaction")
	}
	db.clearGoroutineTx()
	if err := db.commitTx(tx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	db.txMgr.MarkCommitted(tx.xid) // rows inserted by this tx become visible
	return &Result{Message: "COMMIT"}, nil
}

// execRollback must be called with db.mu.Lock() held.
func (db *DB) execRollback() (*Result, error) {
	tx := db.goroutineTx()
	if tx == nil {
		return nil, fmt.Errorf("no active transaction")
	}
	db.clearGoroutineTx()
	return &Result{Message: "ROLLBACK"}, db.rollbackTx(tx)
}

// commitTx logs all dirty pages to the WAL, fsyncs, then flushes them via BufPager.
func (db *DB) commitTx(tx *activeTx) error {
	for key, txpg := range tx.pagers {
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

// getOrOpenTable returns the BufPager for the named table.
// Safe to call from multiple goroutines; uses sync.Map for the registry.
// If two goroutines race to open the same table, the loser discards its handle.
func (db *DB) getOrOpenTable(name string) (*bufferpool.BufPager, error) {
	key := strings.ToLower(name)
	if v, ok := db.openTbls.Load(key); ok {
		return v.(*openTable).bp, nil
	}
	pg, err := pager.Open(db.tablePath(name))
	if err != nil {
		return nil, fmt.Errorf("open table %q: %w", name, err)
	}
	fid := db.pool.Register(pg)
	bp := bufferpool.NewBufPager(db.pool, pg, fid)
	ot := &openTable{pg: pg, fid: fid, bp: bp}
	if actual, loaded := db.openTbls.LoadOrStore(key, ot); loaded {
		// Another goroutine stored first; discard our copy.
		db.pool.Unregister(fid)
		_ = pg.Close()
		return actual.(*openTable).bp, nil
	}
	return bp, nil
}

// txPagerForTable returns the TxPager for the named table in the current
// goroutine's active transaction. Must be called with db.mu.Lock() held.
func (db *DB) txPagerForTable(name string) (*pager.TxPager, error) {
	tx := db.goroutineTx()
	key := strings.ToLower(name)
	if txpg, ok := tx.pagers[key]; ok {
		return txpg, nil
	}
	bp, err := db.getOrOpenTable(name)
	if err != nil {
		return nil, err
	}
	txpg := pager.NewTxPager(bp)
	tx.pagers[key] = txpg
	return txpg, nil
}

// getOrOpenIndex returns the BufPager for the named index.
// Safe to call from multiple goroutines; uses sync.Map for the registry.
func (db *DB) getOrOpenIndex(indexName string) (*bufferpool.BufPager, error) {
	key := strings.ToLower(indexName)
	if v, ok := db.openIdxs.Load(key); ok {
		return v.(*openTable).bp, nil
	}
	pg, err := pager.Open(db.indexPath(indexName))
	if err != nil {
		return nil, fmt.Errorf("open index file %q: %w", indexName, err)
	}
	fid := db.pool.Register(pg)
	bp := bufferpool.NewBufPager(db.pool, pg, fid)
	ot := &openTable{pg: pg, fid: fid, bp: bp}
	if actual, loaded := db.openIdxs.LoadOrStore(key, ot); loaded {
		db.pool.Unregister(fid)
		_ = pg.Close()
		return actual.(*openTable).bp, nil
	}
	return bp, nil
}

// txPagerForIndex returns the TxPager for the named index in the active
// transaction. Must be called with db.mu.Lock() held.
func (db *DB) txPagerForIndex(indexName string) (*pager.TxPager, error) {
	tx := db.goroutineTx()
	key := "idx:" + strings.ToLower(indexName)
	if txpg, ok := tx.pagers[key]; ok {
		return txpg, nil
	}
	bp, err := db.getOrOpenIndex(indexName)
	if err != nil {
		return nil, err
	}
	txpg := pager.NewTxPager(bp)
	tx.pagers[key] = txpg
	return txpg, nil
}

// --- CREATE TABLE ---

// execCreate must be called with db.mu.Lock() held.
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

// execCreateIndex must be called with db.mu.Lock() held.
func (db *DB) execCreateIndex(s *query.CreateIndexStmt) (*Result, error) {
	def := catalog.IndexDef{
		Name:   s.IndexName,
		Table:  s.TableName,
		Column: s.Column,
	}
	if err := db.catalog.CreateIndex(def); err != nil {
		return nil, err
	}
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

// execDropIndex must be called with db.mu.Lock() held.
func (db *DB) execDropIndex(s *query.DropIndexStmt) (*Result, error) {
	key := strings.ToLower(s.IndexName)
	if v, ok := db.openIdxs.Load(key); ok {
		ot := v.(*openTable)
		db.pool.Unregister(ot.fid)
		_ = ot.pg.Close()
		db.openIdxs.Delete(key)
	}
	if err := db.catalog.DropIndex(s.IndexName); err != nil {
		return nil, err
	}
	if err := os.Remove(db.indexPath(s.IndexName)); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove index file: %w", err)
	}
	return &Result{Message: fmt.Sprintf("index %q dropped", s.IndexName)}, nil
}

// --- ANALYZE ---

// execAnalyze must be called with db.mu.Lock() held.
func (db *DB) execAnalyze(s *query.AnalyzeStmt) (*Result, error) {
	tbl, ok := db.catalog.GetTable(s.TableName)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", s.TableName)
	}
	ps, err := db.getOrOpenTable(s.TableName)
	if err != nil {
		return nil, err
	}
	ts, err := stats.Collect(tbl, ps)
	if err != nil {
		return nil, fmt.Errorf("analyze %q: %w", s.TableName, err)
	}
	db.statsDB.Set(ts)
	if err := db.statsDB.Save(); err != nil {
		return nil, fmt.Errorf("save stats: %w", err)
	}
	return &Result{Message: fmt.Sprintf("analyzed %q: %d rows", s.TableName, ts.RowCount)}, nil
}

// --- INSERT ---

// execInsert acquires db.mu.Lock for auto-commit; for explicit transactions the
// lock is already held by the surrounding BEGIN…COMMIT block dispatch.
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

	if tx := db.goroutineTx(); tx != nil {
		// Inside explicit transaction — lock already held by Exec dispatch.
		encoded := encodeRow(tbl, s.Values, tx.xid)
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
		if err := db.insertIntoIndexes(tbl, s.Values, key); err != nil {
			return nil, err
		}
		return &Result{Message: "1 row inserted"}, nil
	}

	// Auto-commit: acquire write lock for the whole operation.
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.autoCommitInsert(tbl, s.TableName, key, s.Values)
}

func (db *DB) autoCommitInsert(tbl *catalog.Table, tableName string, key uint64, values []catalog.Value) (*Result, error) {
	xid, err := db.wal.AllocXID()
	if err != nil {
		return nil, err
	}

	encoded := encodeRow(tbl, values, xid)

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

		if _, found, _ := idxBT.Search(idxKey); found {
			for _, p := range pagers {
				_ = p.Rollback()
			}
			_ = idxTxpg.Rollback()
			_ = db.wal.AppendRollback(xid)
			return nil, fmt.Errorf("unique constraint violated: index %q already has an entry for %s=%d",
				idx.Name, idx.Column, idxKey)
		}

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
	// Make the newly inserted row visible to snapshots taken after this point.
	db.txMgr.MarkCommitted(xid)
	return &Result{Message: "1 row inserted"}, nil
}

// insertIntoIndexes updates all secondary indexes inside an active explicit
// transaction. Must be called with db.mu.Lock() held.
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

func (db *DB) execSelect(s *query.SelectStmt) (*Result, error) {
	// For an explicit transaction, re-use the snapshot taken at BEGIN so the
	// transaction sees a consistent view across all its statements.
	if tx := db.goroutineTx(); tx != nil {
		return db.execSelectWithSnap(s, tx.snap)
	}
	// Auto-commit SELECT: take a fresh snapshot of currently committed XIDs.
	snap := db.txMgr.TakeSnapshot(mvcc.XIDNone)
	return db.execSelectWithSnap(s, snap)
}

func (db *DB) execSelectWithSnap(s *query.SelectStmt, snap mvcc.Snapshot) (*Result, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	// Aggregate queries (any agg function or GROUP BY) use a separate path that
	// materialises all rows, groups them, and computes aggregate values.
	if selectHasAgg(s) {
		return db.execAggSelect(s, snap)
	}

	// When ORDER BY is present, the planner must not apply LIMIT early —
	// we need all rows before sorting, then apply LIMIT ourselves.
	planStmt := s
	if len(s.OrderBy) > 0 && s.Limit > 0 {
		tmp := *s
		tmp.Limit = 0
		planStmt = &tmp
	}

	tables, statsMap, err := db.collectTablesForSelect(planStmt)
	if err != nil {
		return nil, err
	}
	plan, err := planner.Plan(planStmt, tables, statsMap)
	if err != nil {
		return nil, err
	}
	rows, err := execute(plan, db, snap)
	if err != nil {
		return nil, err
	}
	proj := plan.(*planner.Project)
	colNames := proj.Columns

	// ORDER BY: sort, then apply LIMIT.
	if len(s.OrderBy) > 0 {
		rows, err = applyOrderBy(rows, colNames, s.OrderBy)
		if err != nil {
			return nil, err
		}
		if s.Limit > 0 && len(rows) > s.Limit {
			rows = rows[:s.Limit]
		}
	}
	return &Result{Columns: colNames, Rows: rows}, nil
}

func (db *DB) execExplain(s *query.ExplainStmt) (*Result, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	tables, statsMap, err := db.collectTablesForSelect(s.Inner)
	if err != nil {
		return nil, err
	}
	plan, err := planner.Plan(s.Inner, tables, statsMap)
	if err != nil {
		return nil, err
	}
	return &Result{Message: planner.Explain(plan)}, nil
}

func (db *DB) collectTablesForSelect(s *query.SelectStmt) ([]*catalog.Table, map[string]*stats.TableStats, error) {
	refs := make([]query.TableRef, 0, len(s.From)+len(s.Joins))
	refs = append(refs, s.From...)
	for _, j := range s.Joins {
		refs = append(refs, j.Table)
	}

	tables := make([]*catalog.Table, 0, len(refs))
	statsMap := make(map[string]*stats.TableStats, len(refs))

	for _, ref := range refs {
		tbl, ok := db.catalog.GetTable(ref.Name)
		if !ok {
			return nil, nil, fmt.Errorf("table %q does not exist", ref.Name)
		}
		tables = append(tables, tbl)
		if ts, ok := db.statsDB.Get(ref.Name); ok {
			statsMap[strings.ToLower(ref.Name)] = ts
		}
	}
	return tables, statsMap, nil
}

// pageStoreFor returns the correct PageStore for a table:
// TxPager inside an explicit transaction (read-your-own-writes),
// BufPager otherwise (pool-cached concurrent reads).
func (db *DB) pageStoreFor(tableName string) (pager.PageStore, error) {
	if db.goroutineTx() != nil {
		return db.txPagerForTable(tableName)
	}
	return db.getOrOpenTable(tableName)
}

// --- row encoding / decoding ---

// encodeRow encodes user column values into a B-Tree value slot.
// The 8-byte MVCC header (xmin=xid, xmax=0) occupies bytes 0–7.
// User column data follows in bytes 8–71.
func encodeRow(tbl *catalog.Table, values []catalog.Value, xid uint32) [btree.ValueSize]byte {
	var buf [btree.ValueSize]byte
	binary.LittleEndian.PutUint32(buf[0:4], xid)         // xmin
	binary.LittleEndian.PutUint32(buf[4:8], mvcc.XIDNone) // xmax = 0 (live)
	off := mvcc.HeaderSize
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

// decodeRow decodes user column values from a B-Tree value slot,
// skipping the 8-byte MVCC header at the front.
func decodeRow(tbl *catalog.Table, buf [btree.ValueSize]byte) []catalog.Value {
	row := make([]catalog.Value, len(tbl.Columns))
	off := mvcc.HeaderSize // skip xmin + xmax
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
	if size > userDataSize {
		return fmt.Errorf("table %q: row size %d bytes exceeds available space %d (ValueSize=%d minus %d-byte MVCC header)",
			tbl.Name, size, userDataSize, btree.ValueSize, mvcc.HeaderSize)
	}
	return nil
}

func (db *DB) tablePath(name string) string {
	return filepath.Join(db.dir, strings.ToLower(name)+".db")
}

func (db *DB) indexPath(name string) string {
	return filepath.Join(db.dir, strings.ToLower(name)+".idx")
}

// Tables returns all tables in the catalog, sorted alphabetically by name.
func (db *DB) Tables() []*catalog.Table {
	db.mu.RLock()
	defer db.mu.RUnlock()
	tables := db.catalog.Tables()
	sort.Slice(tables, func(i, j int) bool {
		return strings.ToLower(tables[i].Name) < strings.ToLower(tables[j].Name)
	})
	return tables
}

// TableStats returns collected statistics for the named table, if available.
func (db *DB) TableStats(name string) (*stats.TableStats, bool) {
	return db.statsDB.Get(name)
}
