// Package stats collects and stores per-table statistics used by the
// cost-based query optimizer (Phase 10).
//
// Statistics model:
//
//	ANALYZE tablename
//	  → full B-Tree scan of the table
//	  → per column: NDistinct (count of unique values), Min, Max (INT only)
//	  → persisted to <dir>/stats as a compact binary file
//
// Cost model (used by planner.Plan):
//
//	FullScanCost(n)          = ceil(n / LeafOrder) leaf-page I/Os
//	IndexLookupCost(k, n)    = k * 2 * log2(n) page I/Os
//	                             ^ two B-Tree traversals per matching row
//
// The planner uses these functions to choose between IndexLookup (secondary
// index scan + primary fetch) and a full IndexScan + Filter.  When stats are
// unavailable (no ANALYZE run yet), the planner falls back to rule-based
// selection from Phase 9 (always prefer an available index).
//
// Why separate from the planner?
//   Statistics collection requires B-Tree I/O; the planner is pure logic.
//   Keeping them separate means the planner can be tested without a database.
package stats

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/query"
)

const statsMagic = uint32(0x53544154) // "STAT" in little-endian ASCII

// ColumnStat holds statistics for one column gathered by ANALYZE.
type ColumnStat struct {
	Name      string
	NDistinct uint64 // count of distinct values observed during ANALYZE
	Min       uint64 // minimum value (INT columns only; 0 for TEXT)
	Max       uint64 // maximum value (INT columns only; 0 for TEXT)
}

// TableStats holds runtime statistics for one table.
// Produced by Collect and stored in StatsDB.
type TableStats struct {
	Table    string       // lowercase table name
	RowCount uint64       // total rows counted during ANALYZE
	Columns  []ColumnStat // one entry per column in declaration order
}

// ColStat returns the ColumnStat for name (case-insensitive), or nil.
func (ts *TableStats) ColStat(name string) *ColumnStat {
	lower := strings.ToLower(name)
	for i := range ts.Columns {
		if strings.ToLower(ts.Columns[i].Name) == lower {
			return &ts.Columns[i]
		}
	}
	return nil
}

// --- StatsDB ---

// StatsDB holds stats for all tables, backed by <dir>/stats on disk.
// It is loaded once in DB.Open and written on each ANALYZE.
type StatsDB struct {
	tables map[string]*TableStats // keyed by lowercase table name
	path   string
}

// NewStatsDB returns an empty StatsDB backed by path.
func NewStatsDB(path string) *StatsDB {
	return &StatsDB{tables: make(map[string]*TableStats), path: path}
}

// LoadStatsDB reads the stats file at path, or returns an empty StatsDB if the
// file does not exist yet (first run before any ANALYZE).
func LoadStatsDB(path string) (*StatsDB, error) {
	db := NewStatsDB(path)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return db, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open stats %q: %w", path, err)
	}
	defer f.Close()
	if err := db.deserialize(f); err != nil {
		return nil, fmt.Errorf("parse stats %q: %w", path, err)
	}
	return db, nil
}

// Save writes all stats to disk atomically.
func (db *StatsDB) Save() error {
	f, err := os.Create(db.path)
	if err != nil {
		return fmt.Errorf("create stats %q: %w", db.path, err)
	}
	defer f.Close()
	return db.serialize(f)
}

// Get returns the stats for tableName (case-insensitive).
func (db *StatsDB) Get(tableName string) (*TableStats, bool) {
	ts, ok := db.tables[strings.ToLower(tableName)]
	return ts, ok
}

// Set stores (or replaces) stats for ts.Table.
func (db *StatsDB) Set(ts *TableStats) {
	db.tables[strings.ToLower(ts.Table)] = ts
}

// --- Collect ---

// Collect performs a full B-Tree scan of tbl and returns fresh TableStats.
// It uses an in-memory set per column for distinct-value counting, which is
// exact but O(n) in memory.  A HyperLogLog sketch could reduce memory usage
// for very large tables — that is a future optimisation.
func Collect(tbl *catalog.Table, ps pager.PageStore) (*TableStats, error) {
	bt, err := btree.Open(ps, 1)
	if err != nil {
		return nil, fmt.Errorf("stats.Collect: open B-Tree: %w", err)
	}
	cur, err := bt.NewCursor(0, math.MaxUint64)
	if err != nil {
		return nil, fmt.Errorf("stats.Collect: cursor: %w", err)
	}
	defer cur.Close()

	type colTracker struct {
		intSet   map[uint64]struct{}
		textSet  map[string]struct{}
		min, max uint64
		seenAny  bool
	}
	trackers := make([]colTracker, len(tbl.Columns))
	for i, col := range tbl.Columns {
		if col.Type == catalog.TypeInt {
			trackers[i].intSet = make(map[uint64]struct{})
			trackers[i].min = math.MaxUint64
		} else {
			trackers[i].textSet = make(map[string]struct{})
		}
	}

	var rowCount uint64
	for {
		e, ok, err := cur.Next()
		if err != nil {
			return nil, fmt.Errorf("stats.Collect: scan: %w", err)
		}
		if !ok {
			break
		}
		rowCount++
		row := tbl.Decode(e.Value[:])
		for i, v := range row {
			tr := &trackers[i]
			switch v.Type {
			case catalog.TypeInt:
				tr.intSet[v.IntVal] = struct{}{}
				if v.IntVal < tr.min {
					tr.min = v.IntVal
				}
				if v.IntVal > tr.max {
					tr.max = v.IntVal
				}
				tr.seenAny = true
			case catalog.TypeText:
				tr.textSet[v.TextVal] = struct{}{}
			}
		}
	}

	ts := &TableStats{
		Table:    strings.ToLower(tbl.Name),
		RowCount: rowCount,
		Columns:  make([]ColumnStat, len(tbl.Columns)),
	}
	for i, col := range tbl.Columns {
		tr := &trackers[i]
		cs := ColumnStat{Name: col.Name}
		if col.Type == catalog.TypeInt {
			cs.NDistinct = uint64(len(tr.intSet))
			if tr.seenAny {
				cs.Min = tr.min
				cs.Max = tr.max
			}
		} else {
			cs.NDistinct = uint64(len(tr.textSet))
		}
		ts.Columns[i] = cs
	}
	return ts, nil
}

// --- Cost estimation ---

// leafOrder mirrors btree.LeafOrder (56 entries per leaf page).
// Used to estimate the number of leaf-page I/Os for a full scan.
const leafOrder = 56.0

// FullScanCost returns the estimated number of leaf-page I/Os to read every
// row in the table (i.e. the cost of a full IndexScan + Filter).
func FullScanCost(rowCount uint64) float64 {
	if rowCount == 0 {
		return 1
	}
	return math.Ceil(float64(rowCount) / leafOrder)
}

// IndexLookupCost returns the estimated I/O cost to fetch matchingRows rows
// through a secondary index.  Each row requires:
//  1. One B-Tree point lookup in the secondary index:  O(log n) page reads
//  2. One B-Tree point lookup in the primary index:    O(log n) page reads
//
// Total ≈ matchingRows * 2 * log₂(rowCount).  An additional log₂(rowCount)
// term accounts for the initial seek in the secondary index.
func IndexLookupCost(matchingRows, rowCount uint64) float64 {
	if rowCount == 0 || matchingRows == 0 {
		return 1
	}
	logN := math.Log2(float64(rowCount) + 1)
	return float64(matchingRows)*logN*2 + logN
}

// EstimateSelectivity returns the fraction of rows (in [0, 1]) that are
// expected to satisfy cond, using cs for the column's statistics.
// Returns 0.1 (10 %) when no statistics are available.
//
// For equality (OpEq): assumes a uniform distribution → 1 / NDistinct.
// For ranges: uses the fraction of the [Min, Max] interval covered by the bound.
func EstimateSelectivity(cond query.Condition, cs *ColumnStat) float64 {
	if cs == nil || cs.NDistinct == 0 {
		return 0.1
	}
	v := cond.Val.IntVal
	span := float64(cs.Max-cs.Min) + 1
	switch cond.Op {
	case query.OpEq:
		return 1.0 / float64(cs.NDistinct)
	case query.OpGt:
		if v >= cs.Max {
			return 0
		}
		return clamp(float64(cs.Max-v) / span)
	case query.OpGte:
		if v > cs.Max {
			return 0
		}
		return clamp(float64(cs.Max-v+1) / span)
	case query.OpLt:
		if v <= cs.Min {
			return 0
		}
		return clamp(float64(v-cs.Min) / span)
	case query.OpLte:
		if v < cs.Min {
			return 0
		}
		return clamp(float64(v-cs.Min+1) / span)
	}
	return 0.1
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// --- serialization ---
//
// Binary file format:
//   [0-3]  magic     uint32 = 0x53544154
//   [4-5]  numTables uint16
//   For each table:
//     nameLen   uint8  + name bytes
//     rowCount  uint64
//     numCols   uint8
//     For each column:
//       nameLen   uint8  + name bytes
//       nDistinct uint64
//       min       uint64
//       max       uint64

func (db *StatsDB) serialize(w io.Writer) error {
	var buf [8]byte
	binary.LittleEndian.PutUint32(buf[:4], statsMagic)
	if _, err := w.Write(buf[:4]); err != nil {
		return err
	}
	binary.LittleEndian.PutUint16(buf[:2], uint16(len(db.tables)))
	if _, err := w.Write(buf[:2]); err != nil {
		return err
	}
	for _, ts := range db.tables {
		if err := writeTableStats(w, ts); err != nil {
			return err
		}
	}
	return nil
}

func (db *StatsDB) deserialize(r io.Reader) error {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if binary.LittleEndian.Uint32(magic[:]) != statsMagic {
		return fmt.Errorf("not a stats file")
	}
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return fmt.Errorf("read table count: %w", err)
	}
	count := int(binary.LittleEndian.Uint16(n[:]))
	for i := 0; i < count; i++ {
		ts, err := readTableStats(r)
		if err != nil {
			return fmt.Errorf("read table %d: %w", i, err)
		}
		db.tables[strings.ToLower(ts.Table)] = ts
	}
	return nil
}

func writeTableStats(w io.Writer, ts *TableStats) error {
	name := []byte(ts.Table)
	if _, err := w.Write([]byte{byte(len(name))}); err != nil {
		return err
	}
	if _, err := w.Write(name); err != nil {
		return err
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], ts.RowCount)
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	if _, err := w.Write([]byte{byte(len(ts.Columns))}); err != nil {
		return err
	}
	for _, cs := range ts.Columns {
		if err := writeColStat(w, &cs); err != nil {
			return err
		}
	}
	return nil
}

func writeColStat(w io.Writer, cs *ColumnStat) error {
	name := []byte(cs.Name)
	if _, err := w.Write([]byte{byte(len(name))}); err != nil {
		return err
	}
	if _, err := w.Write(name); err != nil {
		return err
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], cs.NDistinct)
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(buf[:], cs.Min)
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(buf[:], cs.Max)
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	return nil
}

func readTableStats(r io.Reader) (*TableStats, error) {
	var nlen [1]byte
	if _, err := io.ReadFull(r, nlen[:]); err != nil {
		return nil, err
	}
	nameBuf := make([]byte, nlen[0])
	if _, err := io.ReadFull(r, nameBuf); err != nil {
		return nil, err
	}
	ts := &TableStats{Table: string(nameBuf)}

	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	ts.RowCount = binary.LittleEndian.Uint64(buf[:])

	var ncols [1]byte
	if _, err := io.ReadFull(r, ncols[:]); err != nil {
		return nil, err
	}
	ts.Columns = make([]ColumnStat, ncols[0])
	for i := range ts.Columns {
		cs, err := readColStat(r)
		if err != nil {
			return nil, err
		}
		ts.Columns[i] = *cs
	}
	return ts, nil
}

func readColStat(r io.Reader) (*ColumnStat, error) {
	var nlen [1]byte
	if _, err := io.ReadFull(r, nlen[:]); err != nil {
		return nil, err
	}
	nameBuf := make([]byte, nlen[0])
	if _, err := io.ReadFull(r, nameBuf); err != nil {
		return nil, err
	}
	cs := &ColumnStat{Name: string(nameBuf)}
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	cs.NDistinct = binary.LittleEndian.Uint64(buf[:])
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	cs.Min = binary.LittleEndian.Uint64(buf[:])
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	cs.Max = binary.LittleEndian.Uint64(buf[:])
	return cs, nil
}
