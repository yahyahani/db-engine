package executor

// dml.go — DELETE and UPDATE implementation (Phase 14).
//
// Design
//
// Both operations share a two-phase approach:
//   1. Scan phase  — iterate the primary B-Tree, filter by MVCC snapshot and
//                    WHERE predicate, collect matching (pk, values) pairs.
//   2. Modify phase — apply xmax / new-values writes and update secondary indexes.
//
// Splitting scan from modify is important because the cursor holds an
// in-memory copy of the current leaf.  If we modified the tree while the
// cursor was live on the same page the cursor's decoded copy would be stale.
// By collecting all targets first we avoid that hazard entirely.
//
// MVCC semantics
//
//   DELETE marks the primary row as dead by writing xmax = current xid.
//          The row remains in the B-Tree but is invisible to snapshots that
//          include that xid.  Secondary index entries are physically removed
//          so that a subsequent INSERT with the same indexed value succeeds.
//
//   UPDATE writes new column values with xmin = current xid and xmax = 0.
//          From the snapshot perspective the "old" row disappears (its xmin
//          is unchanged but the new xmin now gates visibility) and a "new"
//          row appears once the transaction commits.  This is a simplified
//          model; a full MVCC engine would keep both versions simultaneously.

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/mvcc"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/query"
)

// matchesWhere evaluates a WHERE clause (DNF) against a decoded row.
// It reuses evalPreds from operators.go (same package).
func matchesWhere(row []catalog.Value, tbl *catalog.Table, where *query.WhereClause) bool {
	if where == nil {
		return true
	}
	for _, group := range where.Groups {
		if evalPreds(row, tbl, group) {
			return true
		}
	}
	return false
}

// scannedRow is one row collected during the scan phase.
type scannedRow struct {
	pk     uint64
	values []catalog.Value
}

// scanVisible collects all rows in bt that are (a) visible to snap and (b) match where.
func scanVisible(bt *btree.BTree, tbl *catalog.Table, snap mvcc.Snapshot, where *query.WhereClause) ([]scannedRow, error) {
	cur, err := bt.NewCursor(0, math.MaxUint64)
	if err != nil {
		return nil, fmt.Errorf("scan: open cursor: %w", err)
	}
	defer cur.Close()

	var rows []scannedRow
	for {
		e, ok, err := cur.Next()
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if !ok {
			break
		}
		xmin := binary.LittleEndian.Uint32(e.Value[0:4])
		xmax := binary.LittleEndian.Uint32(e.Value[4:8])
		if !snap.IsVisible(xmin, xmax) {
			continue
		}
		vals := decodeRow(tbl, e.Value)
		if !matchesWhere(vals, tbl, where) {
			continue
		}
		rows = append(rows, scannedRow{pk: e.Key, values: vals})
	}
	return rows, nil
}

// idxBTProvider is a lazily-opened cache of index B-Trees, used by both
// applyDelete and applyUpdate to avoid opening the same index twice.
type idxBTProvider struct {
	cache map[string]*btree.BTree
	open  func(indexName string) (*btree.BTree, error)
}

func newIdxBTProvider(open func(string) (*btree.BTree, error)) *idxBTProvider {
	return &idxBTProvider{cache: make(map[string]*btree.BTree), open: open}
}

func (p *idxBTProvider) get(indexName string) (*btree.BTree, error) {
	lower := strings.ToLower(indexName)
	if bt, ok := p.cache[lower]; ok {
		return bt, nil
	}
	bt, err := p.open(indexName)
	if err != nil {
		return nil, err
	}
	p.cache[lower] = bt
	return bt, nil
}

// ─── DELETE ──────────────────────────────────────────────────────────────────

func (db *DB) execDelete(s *query.DeleteStmt) (*Result, error) {
	tbl, ok := db.catalog.GetTable(s.TableName)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", s.TableName)
	}
	if tx := db.goroutineTx(); tx != nil {
		return db.deleteTx(tbl, s.TableName, s.Where, tx)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.autoCommitDelete(tbl, s.TableName, s.Where)
}

func (db *DB) deleteTx(tbl *catalog.Table, tableName string, where *query.WhereClause, tx *activeTx) (*Result, error) {
	txpg, err := db.txPagerForTable(tableName)
	if err != nil {
		return nil, err
	}
	bt, err := btree.Open(txpg, 1)
	if err != nil {
		return nil, fmt.Errorf("open B-Tree: %w", err)
	}
	rows, err := scanVisible(bt, tbl, tx.snap, where)
	if err != nil {
		return nil, err
	}
	idxProv := newIdxBTProvider(func(indexName string) (*btree.BTree, error) {
		idxTxpg, err := db.txPagerForIndex(indexName)
		if err != nil {
			return nil, err
		}
		return btree.Open(idxTxpg, 1)
	})
	n, err := applyDelete(bt, tbl, tx.xid, rows, idxProv)
	if err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("%d row(s) deleted", n)}, nil
}

func (db *DB) autoCommitDelete(tbl *catalog.Table, tableName string, where *query.WhereClause) (*Result, error) {
	xid, err := db.wal.AllocXID()
	if err != nil {
		return nil, err
	}
	snap := db.txMgr.TakeSnapshot(mvcc.XIDNone)

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
		return nil, fmt.Errorf("open B-Tree: %w", err)
	}

	// The pagers map will hold txpg + any index pagers opened during deletion.
	pagers := map[string]*pager.TxPager{strings.ToLower(tableName): txpg}
	idxPagerCache := make(map[string]*pager.TxPager)

	rollbackAll := func() {
		for _, p := range pagers {
			_ = p.Rollback()
		}
		for _, p := range idxPagerCache {
			_ = p.Rollback()
		}
	}

	rows, err := scanVisible(bt, tbl, snap, where)
	if err != nil {
		rollbackAll()
		_ = db.wal.AppendRollback(xid)
		return nil, err
	}

	idxProv := newIdxBTProvider(func(indexName string) (*btree.BTree, error) {
		lower := strings.ToLower(indexName)
		if p, ok := idxPagerCache[lower]; ok {
			return btree.Open(p, 1)
		}
		idxBP, err := db.getOrOpenIndex(indexName)
		if err != nil {
			return nil, fmt.Errorf("open index %q: %w", indexName, err)
		}
		idxTxpg := pager.NewTxPager(idxBP)
		idxPagerCache[lower] = idxTxpg
		return btree.Open(idxTxpg, 1)
	})

	n, err := applyDelete(bt, tbl, xid, rows, idxProv)
	if err != nil {
		rollbackAll()
		_ = db.wal.AppendRollback(xid)
		return nil, err
	}

	for name, p := range idxPagerCache {
		pagers["idx:"+name] = p
	}

	txObj := &activeTx{xid: xid, pagers: pagers}
	if err := db.commitTx(txObj); err != nil {
		rollbackAll()
		return nil, err
	}
	db.txMgr.MarkCommitted(xid)
	return &Result{Message: fmt.Sprintf("%d row(s) deleted", n)}, nil
}

// applyDelete sets xmax on each primary row and removes its secondary index entries.
func applyDelete(bt *btree.BTree, tbl *catalog.Table, xid uint32, rows []scannedRow, idxProv *idxBTProvider) (int, error) {
	for _, r := range rows {
		raw, found, err := bt.Search(r.pk)
		if err != nil {
			return 0, fmt.Errorf("delete: search pk=%d: %w", r.pk, err)
		}
		if !found {
			continue
		}
		binary.LittleEndian.PutUint32(raw[4:8], xid) // xmax = xid
		if err := bt.Insert(r.pk, raw); err != nil {
			return 0, fmt.Errorf("delete: write xmax pk=%d: %w", r.pk, err)
		}
		for _, idx := range tbl.Indexes {
			colIdx := tbl.ColIndex(idx.Column)
			if colIdx < 0 || r.values[colIdx].Type != catalog.TypeInt {
				continue
			}
			idxKey := r.values[colIdx].IntVal
			idxBT, err := idxProv.get(idx.Name)
			if err != nil {
				return 0, err
			}
			if _, err := idxBT.Delete(idxKey); err != nil {
				return 0, fmt.Errorf("delete: remove index %s=%d: %w", idx.Column, idxKey, err)
			}
		}
	}
	return len(rows), nil
}

// ─── UPDATE ──────────────────────────────────────────────────────────────────

// updatePlan is the resolved target column index and new value for one SET clause.
type updatePlan struct {
	colIdx int
	newVal catalog.Value
}

func (db *DB) execUpdate(s *query.UpdateStmt) (*Result, error) {
	tbl, ok := db.catalog.GetTable(s.TableName)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", s.TableName)
	}
	plans, err := buildUpdatePlans(tbl, s.Assignments)
	if err != nil {
		return nil, err
	}
	if tx := db.goroutineTx(); tx != nil {
		return db.updateTx(tbl, s.TableName, s.Where, tx, plans)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.autoCommitUpdate(tbl, s.TableName, s.Where, plans)
}

func buildUpdatePlans(tbl *catalog.Table, assignments []query.Assignment) ([]updatePlan, error) {
	pkIdx := tbl.PrimaryKeyIndex()
	var plans []updatePlan
	for _, a := range assignments {
		colIdx := tbl.ColIndex(a.Column)
		if colIdx < 0 {
			return nil, fmt.Errorf("column %q does not exist in table %q", a.Column, tbl.Name)
		}
		if colIdx == pkIdx {
			return nil, fmt.Errorf("cannot update primary key column %q", a.Column)
		}
		if a.Value.Type != tbl.Columns[colIdx].Type {
			return nil, fmt.Errorf("column %q expects %s, got %s",
				a.Column, tbl.Columns[colIdx].Type, a.Value.Type)
		}
		plans = append(plans, updatePlan{colIdx: colIdx, newVal: a.Value})
	}
	return plans, nil
}

func (db *DB) updateTx(tbl *catalog.Table, tableName string, where *query.WhereClause, tx *activeTx, plans []updatePlan) (*Result, error) {
	txpg, err := db.txPagerForTable(tableName)
	if err != nil {
		return nil, err
	}
	bt, err := btree.Open(txpg, 1)
	if err != nil {
		return nil, fmt.Errorf("open B-Tree: %w", err)
	}
	rows, err := scanVisible(bt, tbl, tx.snap, where)
	if err != nil {
		return nil, err
	}
	idxProv := newIdxBTProvider(func(indexName string) (*btree.BTree, error) {
		idxTxpg, err := db.txPagerForIndex(indexName)
		if err != nil {
			return nil, err
		}
		return btree.Open(idxTxpg, 1)
	})
	n, err := applyUpdate(bt, tbl, tx.xid, rows, plans, idxProv)
	if err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("%d row(s) updated", n)}, nil
}

func (db *DB) autoCommitUpdate(tbl *catalog.Table, tableName string, where *query.WhereClause, plans []updatePlan) (*Result, error) {
	xid, err := db.wal.AllocXID()
	if err != nil {
		return nil, err
	}
	snap := db.txMgr.TakeSnapshot(mvcc.XIDNone)

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
		return nil, fmt.Errorf("open B-Tree: %w", err)
	}

	pagers := map[string]*pager.TxPager{strings.ToLower(tableName): txpg}
	idxPagerCache := make(map[string]*pager.TxPager)

	rollbackAll := func() {
		for _, p := range pagers {
			_ = p.Rollback()
		}
		for _, p := range idxPagerCache {
			_ = p.Rollback()
		}
	}

	rows, err := scanVisible(bt, tbl, snap, where)
	if err != nil {
		rollbackAll()
		_ = db.wal.AppendRollback(xid)
		return nil, err
	}

	idxProv := newIdxBTProvider(func(indexName string) (*btree.BTree, error) {
		lower := strings.ToLower(indexName)
		if p, ok := idxPagerCache[lower]; ok {
			return btree.Open(p, 1)
		}
		idxBP, err := db.getOrOpenIndex(indexName)
		if err != nil {
			return nil, fmt.Errorf("open index %q: %w", indexName, err)
		}
		idxTxpg := pager.NewTxPager(idxBP)
		idxPagerCache[lower] = idxTxpg
		return btree.Open(idxTxpg, 1)
	})

	n, err := applyUpdate(bt, tbl, xid, rows, plans, idxProv)
	if err != nil {
		rollbackAll()
		_ = db.wal.AppendRollback(xid)
		return nil, err
	}

	for name, p := range idxPagerCache {
		pagers["idx:"+name] = p
	}

	txObj := &activeTx{xid: xid, pagers: pagers}
	if err := db.commitTx(txObj); err != nil {
		rollbackAll()
		return nil, err
	}
	db.txMgr.MarkCommitted(xid)
	return &Result{Message: fmt.Sprintf("%d row(s) updated", n)}, nil
}

// applyUpdate writes new column values for each row and updates secondary indexes.
func applyUpdate(bt *btree.BTree, tbl *catalog.Table, xid uint32, rows []scannedRow, plans []updatePlan, idxProv *idxBTProvider) (int, error) {
	for _, r := range rows {
		newVals := make([]catalog.Value, len(r.values))
		copy(newVals, r.values)
		for _, p := range plans {
			newVals[p.colIdx] = p.newVal
		}

		newRaw := encodeRow(tbl, newVals, xid)
		if err := bt.Insert(r.pk, newRaw); err != nil {
			return 0, fmt.Errorf("update: write pk=%d: %w", r.pk, err)
		}

		// Update secondary index entries for any indexed column whose value changed.
		for _, idx := range tbl.Indexes {
			colIdx := tbl.ColIndex(idx.Column)
			if colIdx < 0 || r.values[colIdx].Type != catalog.TypeInt {
				continue
			}
			oldKey := r.values[colIdx].IntVal
			newKey := newVals[colIdx].IntVal
			if oldKey == newKey {
				continue
			}
			idxBT, err := idxProv.get(idx.Name)
			if err != nil {
				return 0, err
			}
			if _, err := idxBT.Delete(oldKey); err != nil {
				return 0, fmt.Errorf("update: remove old index %s=%d: %w", idx.Column, oldKey, err)
			}
			var idxVal [btree.ValueSize]byte
			binary.LittleEndian.PutUint64(idxVal[:8], r.pk)
			if err := idxBT.Insert(newKey, idxVal); err != nil {
				return 0, fmt.Errorf("update: insert new index %s=%d: %w", idx.Column, newKey, err)
			}
		}
	}
	return len(rows), nil
}
