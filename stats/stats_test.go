package stats

import (
	"math"
	"os"
	"testing"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/query"
)

// openTempBTree creates a temporary B-Tree file and returns its pager plus a
// cleanup function.
func openTempBTree(t *testing.T) (pager.PageStore, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "stats-btree-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()

	pg, err := pager.Open(path)
	if err != nil {
		os.Remove(path)
		t.Fatal(err)
	}
	if _, err := btree.Create(pg); err != nil {
		pg.Close()
		os.Remove(path)
		t.Fatal(err)
	}
	return pg, func() {
		pg.Close()
		os.Remove(path)
	}
}

// insertRow encodes and inserts one row into the B-Tree.
func insertRow(t *testing.T, bt *btree.BTree, tbl *catalog.Table, values []catalog.Value) {
	t.Helper()
	pkIdx := tbl.PrimaryKeyIndex()
	if pkIdx < 0 {
		t.Fatal("table has no INT primary key")
	}
	key := values[pkIdx].IntVal

	// Encode the row manually (mirrors executor.encodeRow).
	var buf [btree.ValueSize]byte
	off := 0
	for i, col := range tbl.Columns {
		switch col.Type {
		case catalog.TypeInt:
			v := values[i].IntVal
			for j := 0; j < catalog.IntColSize; j++ {
				buf[off+j] = byte(v >> (8 * j))
			}
			off += catalog.IntColSize
		case catalog.TypeText:
			copy(buf[off:off+catalog.TextColSize], []byte(values[i].TextVal))
			off += catalog.TextColSize
		}
	}
	if err := bt.Insert(key, buf); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// --- Collect tests ---

func TestCollectEmptyTable(t *testing.T) {
	tbl := &catalog.Table{
		Name:    "t",
		Columns: []catalog.ColumnDef{{Name: "id", Type: catalog.TypeInt}},
	}
	ps, cleanup := openTempBTree(t)
	defer cleanup()

	ts, err := Collect(tbl, ps)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if ts.RowCount != 0 {
		t.Errorf("empty table: expected RowCount=0, got %d", ts.RowCount)
	}
}

func TestCollectRowCount(t *testing.T) {
	tbl := &catalog.Table{
		Name:    "nums",
		Columns: []catalog.ColumnDef{{Name: "id", Type: catalog.TypeInt}},
	}
	ps, cleanup := openTempBTree(t)
	defer cleanup()

	bt, _ := btree.Open(ps, 1)
	for i := uint64(1); i <= 10; i++ {
		insertRow(t, bt, tbl, []catalog.Value{{Type: catalog.TypeInt, IntVal: i}})
	}

	ts, err := Collect(tbl, ps)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if ts.RowCount != 10 {
		t.Errorf("expected RowCount=10, got %d", ts.RowCount)
	}
}

func TestCollectIntColumnStats(t *testing.T) {
	tbl := &catalog.Table{
		Name:    "scores",
		Columns: []catalog.ColumnDef{{Name: "id", Type: catalog.TypeInt}, {Name: "score", Type: catalog.TypeInt}},
	}
	ps, cleanup := openTempBTree(t)
	defer cleanup()

	bt, _ := btree.Open(ps, 1)
	for i := uint64(1); i <= 5; i++ {
		insertRow(t, bt, tbl, []catalog.Value{
			{Type: catalog.TypeInt, IntVal: i},
			{Type: catalog.TypeInt, IntVal: i * 10},
		})
	}

	ts, err := Collect(tbl, ps)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	cs := ts.ColStat("score")
	if cs == nil {
		t.Fatal("ColStat('score') returned nil")
	}
	if cs.NDistinct != 5 {
		t.Errorf("NDistinct: got %d, want 5", cs.NDistinct)
	}
	if cs.Min != 10 || cs.Max != 50 {
		t.Errorf("Min/Max: got %d/%d, want 10/50", cs.Min, cs.Max)
	}
}

func TestCollectTextColumnDistinct(t *testing.T) {
	tbl := &catalog.Table{
		Name:    "words",
		Columns: []catalog.ColumnDef{{Name: "id", Type: catalog.TypeInt}, {Name: "word", Type: catalog.TypeText}},
	}
	ps, cleanup := openTempBTree(t)
	defer cleanup()

	bt, _ := btree.Open(ps, 1)
	// Insert 6 rows but only 3 distinct words.
	words := []string{"foo", "bar", "baz", "foo", "bar", "baz"}
	for i, w := range words {
		insertRow(t, bt, tbl, []catalog.Value{
			{Type: catalog.TypeInt, IntVal: uint64(i + 1)},
			{Type: catalog.TypeText, TextVal: w},
		})
	}

	ts, err := Collect(tbl, ps)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	cs := ts.ColStat("word")
	if cs == nil {
		t.Fatal("ColStat('word') returned nil")
	}
	if cs.NDistinct != 3 {
		t.Errorf("NDistinct: got %d, want 3", cs.NDistinct)
	}
}

// --- StatsDB persistence ---

func TestStatsDBSaveAndLoad(t *testing.T) {
	f, _ := os.CreateTemp("", "stats-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	db := NewStatsDB(path)
	db.Set(&TableStats{
		Table:    "users",
		RowCount: 1000,
		Columns: []ColumnStat{
			{Name: "id", NDistinct: 1000, Min: 1, Max: 1000},
			{Name: "name", NDistinct: 850},
			{Name: "age", NDistinct: 80, Min: 18, Max: 99},
		},
	})
	if err := db.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	db2, err := LoadStatsDB(path)
	if err != nil {
		t.Fatalf("LoadStatsDB: %v", err)
	}
	ts, ok := db2.Get("users")
	if !ok {
		t.Fatal("users stats not found after reload")
	}
	if ts.RowCount != 1000 {
		t.Errorf("RowCount: got %d, want 1000", ts.RowCount)
	}
	cs := ts.ColStat("age")
	if cs == nil {
		t.Fatal("age ColStat not found after reload")
	}
	if cs.NDistinct != 80 || cs.Min != 18 || cs.Max != 99 {
		t.Errorf("age stats: got NDistinct=%d Min=%d Max=%d", cs.NDistinct, cs.Min, cs.Max)
	}
}

func TestLoadStatsDBNonexistentReturnsEmpty(t *testing.T) {
	db, err := LoadStatsDB("/tmp/definitely-does-not-exist-stats.bin")
	if err != nil {
		t.Fatalf("expected empty StatsDB for nonexistent file, got error: %v", err)
	}
	if _, ok := db.Get("anything"); ok {
		t.Error("expected empty StatsDB")
	}
}

// --- Cost estimation ---

func TestFullScanCost(t *testing.T) {
	// An empty table costs at least 1.
	if got := FullScanCost(0); got != 1 {
		t.Errorf("FullScanCost(0): got %v, want 1", got)
	}
	// 56 rows exactly fill one leaf page.
	if got := FullScanCost(56); got != 1 {
		t.Errorf("FullScanCost(56): got %v, want 1", got)
	}
	// 57 rows need 2 pages.
	if got := FullScanCost(57); got != 2 {
		t.Errorf("FullScanCost(57): got %v, want 2", got)
	}
	// 10 000 rows need ceil(10000/56) = 179 pages.
	want := math.Ceil(10000.0 / 56.0)
	if got := FullScanCost(10_000); got != want {
		t.Errorf("FullScanCost(10000): got %v, want %v", got, want)
	}
}

func TestIndexLookupCostGrowsWithMatchingRows(t *testing.T) {
	// Fetching more rows through an index should always cost more.
	n := uint64(10_000)
	c1 := IndexLookupCost(1, n)
	c100 := IndexLookupCost(100, n)
	c1000 := IndexLookupCost(1000, n)
	if !(c1 < c100 && c100 < c1000) {
		t.Errorf("index lookup cost should grow with matchingRows: c1=%v c100=%v c1000=%v", c1, c100, c1000)
	}
}

func TestIndexBeatsFullScanForHighlySelectiveQuery(t *testing.T) {
	// 1 matching row out of 10 000 → index should be much cheaper.
	n := uint64(10_000)
	il := IndexLookupCost(1, n)
	fs := FullScanCost(n)
	if il >= fs {
		t.Errorf("index lookup (%v) should be cheaper than full scan (%v) for 1 matching row in %d", il, fs, n)
	}
}

func TestFullScanBeatsIndexForLowSelectivity(t *testing.T) {
	// 5 000 matching rows out of 10 000 (50%) → full scan wins.
	n := uint64(10_000)
	il := IndexLookupCost(5_000, n)
	fs := FullScanCost(n)
	if il <= fs {
		t.Errorf("full scan (%v) should be cheaper than index lookup (%v) for 5000/%d rows", fs, il, n)
	}
}

// --- EstimateSelectivity ---

func TestSelectivityEq(t *testing.T) {
	cs := &ColumnStat{Name: "age", NDistinct: 100, Min: 0, Max: 99}
	cond := query.Condition{Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 42}}
	got := EstimateSelectivity(cond, cs)
	if math.Abs(got-0.01) > 1e-9 {
		t.Errorf("OpEq with NDistinct=100: got %v, want 0.01", got)
	}
}

func TestSelectivityGt(t *testing.T) {
	cs := &ColumnStat{Name: "score", NDistinct: 100, Min: 0, Max: 99}
	// score > 49: roughly half the range
	cond := query.Condition{Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 49}}
	got := EstimateSelectivity(cond, cs)
	if got <= 0 || got > 0.51 {
		t.Errorf("OpGt(49): got %v, expected ~0.50", got)
	}
}

func TestSelectivityOutOfRange(t *testing.T) {
	cs := &ColumnStat{Name: "x", NDistinct: 10, Min: 10, Max: 20}
	// x > 100 — no rows can match
	cond := query.Condition{Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 100}}
	if got := EstimateSelectivity(cond, cs); got != 0 {
		t.Errorf("out-of-range OpGt: got %v, want 0", got)
	}
}

func TestSelectivityNilColumnStat(t *testing.T) {
	cond := query.Condition{Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 5}}
	got := EstimateSelectivity(cond, nil)
	if got != 0.1 {
		t.Errorf("nil ColumnStat should return 0.1 default, got %v", got)
	}
}
