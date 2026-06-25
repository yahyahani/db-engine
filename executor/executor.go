// Package executor ties the query language (query package) and schema storage
// (catalog package) to the B+ Tree storage engine (btree + pager packages).
//
// Execution pipeline for a SELECT:
//   SQL string → Parse() → *SelectStmt → planKeyRange() → RangeScan/Search
//               → decode rows → post-filter → project columns → Result
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
)

// intColSize and textColSize are the fixed on-disk byte widths for each column type.
//
// Why fixed widths?
//   Variable-length encoding (like PostgreSQL's varlena) is more space-efficient
//   but requires offset arrays and makes random access O(n) within a row.
//   For Phase 3, fixed widths keep encoding dead simple: column offset = sum of
//   prior column widths. Phase 6 can introduce overflow pages for long strings.
//
// Why TEXT = 48 bytes?
//   With INT = 8 bytes and btree.ValueSize = 64, the table [id INT, name TEXT, age INT]
//   uses 8 + 48 + 8 = 64 bytes — exactly fills one B+ Tree value slot.
//   This is a deliberate constraint, not a bug. It teaches how storage engines
//   must balance record size against page capacity.
const (
	intColSize  = 8  // bytes per INT column
	textColSize = 48 // bytes per TEXT column; max 47 printable chars + room for a null
)

// DB is an open database backed by a directory.
// Each table has its own B+ Tree file (<dir>/<table>.db).
// The schema for all tables is in <dir>/catalog.
type DB struct {
	dir     string
	catalog *catalog.Catalog
}

// Result is returned by Exec for every statement.
// For SELECT, Columns and Rows are populated.
// For CREATE TABLE and INSERT, only Message is set.
type Result struct {
	Columns []string          // column headers
	Rows    [][]catalog.Value // decoded row values
	Message string
}

// Open opens (or creates) a database at dir.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create database directory %q: %w", dir, err)
	}
	cat, err := catalog.Load(filepath.Join(dir, "catalog"))
	if err != nil {
		return nil, fmt.Errorf("load catalog: %w", err)
	}
	return &DB{dir: dir, catalog: cat}, nil
}

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
	default:
		return nil, fmt.Errorf("unsupported statement type %T", stmt)
	}
}

// --- CREATE TABLE ---

func (db *DB) execCreate(s *query.CreateTableStmt) (*Result, error) {
	tbl := &catalog.Table{Name: s.TableName, Columns: s.Columns}

	if err := validateSchema(tbl); err != nil {
		return nil, err
	}

	// Create the B+ Tree file for this table.
	pg, err := pager.Open(db.tablePath(s.TableName))
	if err != nil {
		return nil, fmt.Errorf("create table file: %w", err)
	}
	if _, err := btree.Create(pg); err != nil {
		pg.Close()
		return nil, fmt.Errorf("init B+ Tree for %q: %w", s.TableName, err)
	}
	if err := pg.Close(); err != nil {
		return nil, err
	}

	// Add to catalog after the file is ready. If we did it before, a crash
	// between catalog.Save and file creation would leave a phantom table.
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

	pg, err := pager.Open(db.tablePath(s.TableName))
	if err != nil {
		return nil, fmt.Errorf("open table file: %w", err)
	}
	defer pg.Close()

	bt, err := btree.Open(pg, 1)
	if err != nil {
		return nil, fmt.Errorf("open B+ Tree: %w", err)
	}
	if err := bt.Insert(key, encoded); err != nil {
		return nil, fmt.Errorf("insert: %w", err)
	}
	return &Result{Message: "1 row inserted"}, nil
}

// --- SELECT ---

func (db *DB) execSelect(s *query.SelectStmt) (*Result, error) {
	tbl, ok := db.catalog.GetTable(s.TableName)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", s.TableName)
	}

	// Determine which columns to return.
	outCols, colIdxs, err := resolveColumns(tbl, s.Columns)
	if err != nil {
		return nil, err
	}

	// Plan the key range from WHERE conditions on the primary key.
	// This is the "index push-down": instead of scanning all rows and filtering,
	// we compute the tightest B+ Tree range that satisfies the PK conditions.
	// Non-PK conditions become post-filters applied after the scan.
	minKey, maxKey := planKeyRange(tbl, s.Where)

	pg, err := pager.Open(db.tablePath(s.TableName))
	if err != nil {
		return nil, fmt.Errorf("open table file: %w", err)
	}
	defer pg.Close()

	bt, err := btree.Open(pg, 1)
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

		// Post-filter: apply WHERE conditions on non-PK columns (or repeated PK checks).
		if s.Where != nil && !rowMatchesWhere(row, tbl, s.Where) {
			continue
		}

		// Project: pick only the requested columns.
		projected := make([]catalog.Value, len(colIdxs))
		for i, idx := range colIdxs {
			projected[i] = row[idx]
		}
		res.Rows = append(res.Rows, projected)
	}
	return res, nil
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
// Called after the index scan to filter rows that passed the key range but fail
// non-PK conditions (e.g. WHERE name = 'Alice').
func rowMatchesWhere(row []catalog.Value, tbl *catalog.Table, where *query.WhereClause) bool {
	for _, cond := range where.Conds {
		idx := tbl.ColIndex(cond.Column)
		if idx < 0 {
			continue // unknown column — ignore (error could be raised earlier)
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
// Returns (output column names, column indices in schema row).
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

// encodeRow packs a row's values into a fixed [btree.ValueSize]byte.
// Column layout is deterministic: INT = intColSize bytes, TEXT = textColSize bytes,
// in schema column order. Redundantly storing the PK in the value is a
// deliberate simplicity choice: decoding never needs the B+ Tree key separately.
func encodeRow(tbl *catalog.Table, values []catalog.Value) [btree.ValueSize]byte {
	var buf [btree.ValueSize]byte
	off := 0
	for i, col := range tbl.Columns {
		switch col.Type {
		case catalog.TypeInt:
			binary.LittleEndian.PutUint64(buf[off:off+intColSize], values[i].IntVal)
			off += intColSize
		case catalog.TypeText:
			// copy truncates automatically if TextVal is longer than textColSize
			copy(buf[off:off+textColSize], values[i].TextVal)
			off += textColSize
		}
	}
	return buf
}

// decodeRow unpacks a [btree.ValueSize]byte into a slice of Values using the schema.
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
			// Trim trailing zero bytes — TEXT is stored null-padded.
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

// validateSchema checks that a table schema is usable in Phase 3.
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
		return fmt.Errorf("table %q: row size %d bytes exceeds B+ Tree value size %d — "+
			"use fewer columns or smaller types", tbl.Name, size, btree.ValueSize)
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
