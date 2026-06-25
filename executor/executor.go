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
//     5. Flush dirty pages to the data file
//
//   If the process crashes between steps 4 and 5, the WAL replay on next Open()
//   re-applies the committed writes.  If it crashes before step 4, there is no
//   Commit record and recovery skips those writes entirely.
//
//   Explicit transactions (BEGIN / COMMIT / ROLLBACK) keep TxPagers open across
//   multiple statements and commit all dirty pages atomically at COMMIT.
//
// Why separate planning (planKeyRange) from execution (scan)?
//   Even in this minimal implementation, separating "what range to read" from
//   "actually reading it" is the seed of a query planner. Phase 6 will expand
//   this into a proper plan tree with cost estimates.
package executor

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/yahya/db-engine/btree"
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

// DB is an open database backed by a directory.
// Each table has its own B+ Tree file (<dir>/<table>.db).
// The schema for all tables is in <dir>/catalog.
// The WAL is in <dir>/wal.
type DB struct {
	dir      string
	catalog  *catalog.Catalog
	wal      *wal.WAL
	activeTx *activeTx // non-nil when an explicit BEGIN is in progress
}

// activeTx holds the state of an explicit transaction across multiple statements.
type activeTx struct {
	xid    uint32
	pagers map[string]*pager.TxPager // lowercase table name → TxPager
	bases  map[string]*pager.Pager   // lowercase table name → underlying Pager (kept open)
}

// Result is returned by Exec for every statement.
// For SELECT, Columns and Rows are populated.
// For other statements, only Message is set.
type Result struct {
	Columns []string          // column headers
	Rows    [][]catalog.Value // decoded row values
	Message string
}

// Open opens (or creates) a database at dir and runs WAL crash recovery.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create database directory %q: %w", dir, err)
	}

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		return nil, fmt.Errorf("open WAL: %w", err)
	}
	// Redo any committed writes that did not reach the data files before a crash.
	if err := w.Recover(dir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("WAL recovery: %w", err)
	}

	cat, err := catalog.Load(filepath.Join(dir, "catalog"))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("load catalog: %w", err)
	}
	return &DB{dir: dir, catalog: cat, wal: w}, nil
}

// Close rolls back any open transaction, syncs the WAL, and releases file handles.
func (db *DB) Close() error {
	if db.activeTx != nil {
		_ = db.rollbackTx(db.activeTx)
		db.activeTx = nil
	}
	return db.wal.Close()
}

// InTransaction reports whether an explicit BEGIN is in progress.
func (db *DB) InTransaction() bool { return db.activeTx != nil }

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
		bases:  make(map[string]*pager.Pager),
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

// commitTx logs all dirty pages to the WAL, fsyncs, flushes them to disk, then logs COMMIT.
func (db *DB) commitTx(tx *activeTx) error {
	// Log write records for every dirty page in every table.
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
	// Commit record + fsync: this is the durability point.
	// After Sync() returns, the transaction survives any subsequent crash.
	if err := db.wal.AppendCommit(tx.xid); err != nil {
		return err
	}
	if err := db.wal.Sync(); err != nil {
		return err
	}
	// Flush dirty pages to the data files (force policy).
	for _, txpg := range tx.pagers {
		if err := txpg.Flush(); err != nil {
			return err
		}
	}
	for _, pg := range tx.bases {
		if err := pg.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) rollbackTx(tx *activeTx) error {
	for _, txpg := range tx.pagers {
		_ = txpg.Rollback()
	}
	for _, pg := range tx.bases {
		_ = pg.Close()
	}
	return db.wal.AppendRollback(tx.xid)
}

// txPagerForTable returns the TxPager for the given table in the active transaction,
// creating one (and opening the base Pager) on first access.
func (db *DB) txPagerForTable(tableName string) (*pager.TxPager, error) {
	key := strings.ToLower(tableName)
	if txpg, ok := db.activeTx.pagers[key]; ok {
		return txpg, nil
	}
	pg, err := pager.Open(db.tablePath(tableName))
	if err != nil {
		return nil, fmt.Errorf("open table file for tx: %w", err)
	}
	txpg := pager.NewTxPager(pg)
	db.activeTx.pagers[key] = txpg
	db.activeTx.bases[key] = pg
	return txpg, nil
}

// --- CREATE TABLE ---

// execCreate is DDL: always auto-commits outside any explicit transaction.
// We do not WAL-protect the btree file creation — if it fails mid-way the user
// can retry. The catalog is written after the btree file is ready to avoid a
// phantom table with no backing file.
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
		// Inside explicit transaction: buffer writes via TxPager.
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

	// Auto-commit: wrap the single INSERT in a full WAL transaction.
	return db.autoCommitInsert(s.TableName, key, encoded)
}

// autoCommitInsert wraps a single INSERT in a WAL transaction:
//   AllocXID → insert into TxPager → log writes → log commit → fsync → flush pages
func (db *DB) autoCommitInsert(tableName string, key uint64, encoded [btree.ValueSize]byte) (*Result, error) {
	xid, err := db.wal.AllocXID()
	if err != nil {
		return nil, err
	}

	pg, err := pager.Open(db.tablePath(tableName))
	if err != nil {
		_ = db.wal.AppendRollback(xid)
		return nil, fmt.Errorf("open table file: %w", err)
	}

	txpg := pager.NewTxPager(pg)
	bt, err := btree.Open(txpg, 1)
	if err != nil {
		_ = txpg.Rollback()
		_ = pg.Close()
		_ = db.wal.AppendRollback(xid)
		return nil, fmt.Errorf("open B+ Tree: %w", err)
	}

	if err := bt.Insert(key, encoded); err != nil {
		_ = txpg.Rollback()
		_ = pg.Close()
		_ = db.wal.AppendRollback(xid)
		return nil, fmt.Errorf("insert: %w", err)
	}

	// Commit: log dirty pages → log commit → fsync → flush.
	singleTx := &activeTx{
		xid:    xid,
		pagers: map[string]*pager.TxPager{strings.ToLower(tableName): txpg},
		bases:  map[string]*pager.Pager{strings.ToLower(tableName): pg},
	}
	if err := db.commitTx(singleTx); err != nil {
		// commitTx closes bases; rollback the TxPager dirty buffer.
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

	// Choose a PageStore: TxPager (reads own uncommitted writes) inside a tx,
	// or a freshly opened base Pager outside one.
	ps, closePS, err := db.getReadStore(s.TableName)
	if err != nil {
		return nil, err
	}
	defer closePS()

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

// getReadStore returns a PageStore appropriate for a read operation.
// Inside an explicit transaction, returns the TxPager (for read-your-own-writes).
// Outside a transaction, opens and returns a fresh base Pager.
func (db *DB) getReadStore(tableName string) (pager.PageStore, func(), error) {
	if db.activeTx != nil {
		txpg, err := db.txPagerForTable(tableName)
		if err != nil {
			return nil, nil, err
		}
		return txpg, func() {}, nil
	}
	pg, err := pager.Open(db.tablePath(tableName))
	if err != nil {
		return nil, nil, fmt.Errorf("open table file: %w", err)
	}
	return pg, func() { _ = pg.Close() }, nil
}

// planKeyRange computes the tightest (minKey, maxKey) range implied by the
// WHERE conditions on the primary key column. If there is no WHERE or no PK
// condition, it returns (0, MaxUint64) = full table scan.
//
// Example: WHERE id >= 10 AND id < 20 → (10, 19)
// Example: WHERE id = 5             → (5, 5) — point lookup via range scan
// Example: WHERE name = 'Alice'     → (0, MaxUint64) — full scan, post-filter
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
				minKey, maxKey = 1, 0 // empty range
			}
		case query.OpGte:
			minKey = max64(minKey, v)
		case query.OpLt:
			if v > 0 {
				maxKey = min64(maxKey, v-1)
			} else {
				minKey, maxKey = 1, 0 // empty range (nothing < 0 for uint)
			}
		case query.OpLte:
			maxKey = min64(maxKey, v)
		}
	}
	return
}

// rowMatchesWhere tests whether a decoded row satisfies all WHERE conditions.
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

// resolveColumns maps SELECT column list to schema indices.
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
