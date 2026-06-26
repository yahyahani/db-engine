// Package catalog manages table schemas and secondary index definitions.
//
// Why a catalog?
//   The query executor needs to know column names, types, and order for every
//   table before it can encode/decode rows or plan a query. The catalog is the
//   "schema dictionary" that persists this information across restarts.
//   In PostgreSQL this is the pg_catalog system tables; in SQLite it's the
//   sqlite_master table. We use a single binary file to keep it simple.
package catalog

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

// Row encoding sizes.  Every column occupies a fixed number of bytes inside the
// 64-byte B-Tree value slot so the executor and stats packages agree on layout.
const (
	IntColSize  = 8  // bytes per INT column
	TextColSize = 48 // bytes per TEXT column; 47 usable chars + null terminator
)

// catalogMagicV2 is the magic number for the current catalog format.
// Bumped from V1 (0xCA7A1060) when secondary index definitions were added.
// Open() rejects old catalogs with a clear error rather than silently
// mis-reading the index count field as part of column data.
const catalogMagicV2 = uint32(0xCA7A1061)

// DataType is the set of column types supported in Phase 3.
// Defined here (not in the query layer) because the catalog is the ground truth
// for what types are stored — the query parser learns these type names from the
// SQL grammar, but the actual storage semantics live here.
type DataType uint8

const (
	TypeInt  DataType = 0 // 64-bit unsigned integer; 8 bytes on disk
	TypeText DataType = 1 // fixed-width UTF-8 string; TextColSize bytes on disk
)

func (d DataType) String() string {
	if d == TypeInt {
		return "INT"
	}
	return "TEXT"
}

// ColumnDef describes one column in a table schema.
type ColumnDef struct {
	Name string
	Type DataType
}

// Value is a scalar SQL value — used both for query literals and for decoded row fields.
// Having one type for both avoids translation layers between the parser and executor.
type Value struct {
	Type    DataType
	IntVal  uint64 // set when Type == TypeInt
	TextVal string // set when Type == TypeText
}

func (v Value) String() string {
	if v.Type == TypeInt {
		return fmt.Sprintf("%d", v.IntVal)
	}
	return v.TextVal
}

// IndexDef is the metadata for one secondary index.
//
// A secondary index stores (indexed_col_value → primary_key) in its own B-Tree
// file named <dir>/<IndexName>.idx.  Only INT columns may be indexed in Phase 9
// because the B-Tree key type is uint64 and TEXT values have no natural ordering
// that fits in 8 bytes.  Non-unique indexed values are not supported: if two rows
// share an indexed value the second INSERT will fail with a unique-constraint error.
type IndexDef struct {
	Name   string // unique index name across the database (e.g. "idx_users_age")
	Table  string // lowercase table name
	Column string // column name this index covers (must be TypeInt)
}

// Table is the schema for one relation.
type Table struct {
	Name    string
	Columns []ColumnDef
	Indexes []IndexDef // secondary indexes defined on this table
}

// PrimaryKeyIndex returns the index of the first INT column.
// In Phase 3, the first INT column is always the B+ Tree primary key.
// Returns -1 if no INT column exists.
func (t *Table) PrimaryKeyIndex() int {
	for i, c := range t.Columns {
		if c.Type == TypeInt {
			return i
		}
	}
	return -1
}

// ColIndex returns the index of the named column (case-insensitive), or -1.
func (t *Table) ColIndex(name string) int {
	lower := strings.ToLower(name)
	for i, c := range t.Columns {
		if strings.ToLower(c.Name) == lower {
			return i
		}
	}
	return -1
}

// IndexForColumn returns the first IndexDef whose Column matches name
// (case-insensitive), or nil if no index covers that column.
func (t *Table) IndexForColumn(name string) *IndexDef {
	lower := strings.ToLower(name)
	for i := range t.Indexes {
		if strings.ToLower(t.Indexes[i].Column) == lower {
			return &t.Indexes[i]
		}
	}
	return nil
}

// Catalog is an in-memory map of all table schemas, backed by a binary file.
type Catalog struct {
	tables  map[string]*Table    // keyed by lowercase table name
	indexes map[string]*IndexDef // keyed by lowercase index name (for DROP INDEX)
	path    string
}

// New returns an empty Catalog backed by path.
// Call Save() after mutations to persist changes.
func New(path string) *Catalog {
	return &Catalog{
		tables:  make(map[string]*Table),
		indexes: make(map[string]*IndexDef),
		path:    path,
	}
}

// Load reads the catalog from path, or returns an empty Catalog if the file
// doesn't exist yet (first run of a new database).
func Load(path string) (*Catalog, error) {
	c := New(path)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open catalog %q: %w", path, err)
	}
	defer f.Close()

	if err := c.deserialize(f); err != nil {
		return nil, fmt.Errorf("parse catalog %q: %w", path, err)
	}
	return c, nil
}

// Save atomically writes the catalog to disk.
func (c *Catalog) Save() error {
	f, err := os.Create(c.path)
	if err != nil {
		return fmt.Errorf("create catalog %q: %w", c.path, err)
	}
	defer f.Close()
	return c.serialize(f)
}

// CreateTable adds a table to the catalog and saves immediately.
// Returns an error if a table with that name already exists.
func (c *Catalog) CreateTable(t *Table) error {
	key := strings.ToLower(t.Name)
	if _, exists := c.tables[key]; exists {
		return fmt.Errorf("table %q already exists", t.Name)
	}
	c.tables[key] = t
	return c.Save()
}

// GetTable looks up a table by name (case-insensitive).
func (c *Catalog) GetTable(name string) (*Table, bool) {
	t, ok := c.tables[strings.ToLower(name)]
	return t, ok
}

// Tables returns all tables in the catalog.
func (c *Catalog) Tables() []*Table {
	out := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t)
	}
	return out
}

// CreateIndex registers a secondary index definition and saves immediately.
//
// Constraints:
//   - The table must exist.
//   - The column must be of TypeInt (B-Tree key is uint64).
//   - The index name must be unique across the catalog.
//   - Only one index per column per table is allowed in Phase 9.
func (c *Catalog) CreateIndex(idx IndexDef) error {
	tableKey := strings.ToLower(idx.Table)
	tbl, ok := c.tables[tableKey]
	if !ok {
		return fmt.Errorf("table %q does not exist", idx.Table)
	}
	colIdx := tbl.ColIndex(idx.Column)
	if colIdx < 0 {
		return fmt.Errorf("column %q not found in table %q", idx.Column, idx.Table)
	}
	if tbl.Columns[colIdx].Type != TypeInt {
		return fmt.Errorf("column %q is %s; only INT columns may be indexed in Phase 9",
			idx.Column, tbl.Columns[colIdx].Type)
	}
	// Prevent duplicate index names.
	idxKey := strings.ToLower(idx.Name)
	if _, exists := c.indexes[idxKey]; exists {
		return fmt.Errorf("index %q already exists", idx.Name)
	}
	// Prevent two indexes on the same column.
	if existing := tbl.IndexForColumn(idx.Column); existing != nil {
		return fmt.Errorf("column %q already has an index (%q)", idx.Column, existing.Name)
	}

	idx.Table = tableKey
	idx.Column = strings.ToLower(idx.Column)
	tbl.Indexes = append(tbl.Indexes, idx)
	c.indexes[idxKey] = &tbl.Indexes[len(tbl.Indexes)-1]
	return c.Save()
}

// DropIndex removes a secondary index definition and saves immediately.
func (c *Catalog) DropIndex(indexName string) error {
	key := strings.ToLower(indexName)
	def, ok := c.indexes[key]
	if !ok {
		return fmt.Errorf("index %q does not exist", indexName)
	}
	tbl, ok := c.tables[def.Table]
	if !ok {
		return fmt.Errorf("table %q not found for index %q", def.Table, indexName)
	}
	// Remove from table's Indexes slice.
	for i, idx := range tbl.Indexes {
		if strings.ToLower(idx.Name) == key {
			tbl.Indexes = append(tbl.Indexes[:i], tbl.Indexes[i+1:]...)
			break
		}
	}
	delete(c.indexes, key)
	return c.Save()
}

// GetIndex looks up an index by name (case-insensitive).
func (c *Catalog) GetIndex(name string) (*IndexDef, bool) {
	def, ok := c.indexes[strings.ToLower(name)]
	return def, ok
}

// Decode decodes a raw B-Tree value slot ([]byte) into column values according
// to the table's schema.  Used by the stats collector and the executor.
// buf must contain at least the total encoded row size.
func (t *Table) Decode(buf []byte) []Value {
	row := make([]Value, len(t.Columns))
	off := 0
	for i, col := range t.Columns {
		switch col.Type {
		case TypeInt:
			row[i] = Value{Type: TypeInt, IntVal: binary.LittleEndian.Uint64(buf[off : off+IntColSize])}
			off += IntColSize
		case TypeText:
			raw := buf[off : off+TextColSize]
			end := TextColSize
			for end > 0 && raw[end-1] == 0 {
				end--
			}
			row[i] = Value{Type: TypeText, TextVal: string(raw[:end])}
			off += TextColSize
		}
	}
	return row
}

// --- binary serialization ---
//
// File format (magic V2 = 0xCA7A1061):
//
//   [0–3]  magic    uint32 = 0xCA7A1061
//   [4–5]  numTables uint16
//   For each table:
//     nameLen   uint8  + name bytes
//     numCols   uint8
//     For each column:
//       colNameLen uint8 + colName bytes
//       colType    uint8
//     numIndexes uint8
//     For each index:
//       idxNameLen uint8 + idxName bytes
//       colNameLen uint8 + colName bytes
//       (Table name is implicit — it is the current table)

func (c *Catalog) serialize(w io.Writer) error {
	var magic [4]byte
	binary.LittleEndian.PutUint32(magic[:], catalogMagicV2)
	if _, err := w.Write(magic[:]); err != nil {
		return err
	}
	var n [2]byte
	binary.LittleEndian.PutUint16(n[:], uint16(len(c.tables)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	for _, t := range c.tables {
		if err := writeTable(w, t); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) deserialize(r io.Reader) error {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	m := binary.LittleEndian.Uint32(magic[:])
	if m == uint32(0xCA7A1060) {
		return fmt.Errorf("catalog format V1 is no longer supported; recreate the database")
	}
	if m != catalogMagicV2 {
		return fmt.Errorf("bad magic number 0x%08X — file is not a catalog", m)
	}
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return fmt.Errorf("read table count: %w", err)
	}
	count := int(binary.LittleEndian.Uint16(n[:]))
	for i := 0; i < count; i++ {
		t, err := readTable(r)
		if err != nil {
			return fmt.Errorf("read table %d: %w", i, err)
		}
		key := strings.ToLower(t.Name)
		c.tables[key] = t
		for i := range t.Indexes {
			c.indexes[strings.ToLower(t.Indexes[i].Name)] = &t.Indexes[i]
		}
	}
	return nil
}

func writeTable(w io.Writer, t *Table) error {
	name := []byte(t.Name)
	if _, err := w.Write([]byte{byte(len(name))}); err != nil {
		return err
	}
	if _, err := w.Write(name); err != nil {
		return err
	}
	if _, err := w.Write([]byte{byte(len(t.Columns))}); err != nil {
		return err
	}
	for _, c := range t.Columns {
		cn := []byte(c.Name)
		if _, err := w.Write([]byte{byte(len(cn))}); err != nil {
			return err
		}
		if _, err := w.Write(cn); err != nil {
			return err
		}
		if _, err := w.Write([]byte{byte(c.Type)}); err != nil {
			return err
		}
	}
	// Indexes (Phase 9 addition)
	if _, err := w.Write([]byte{byte(len(t.Indexes))}); err != nil {
		return err
	}
	for _, idx := range t.Indexes {
		in := []byte(idx.Name)
		if _, err := w.Write([]byte{byte(len(in))}); err != nil {
			return err
		}
		if _, err := w.Write(in); err != nil {
			return err
		}
		col := []byte(idx.Column)
		if _, err := w.Write([]byte{byte(len(col))}); err != nil {
			return err
		}
		if _, err := w.Write(col); err != nil {
			return err
		}
	}
	return nil
}

func readTable(r io.Reader) (*Table, error) {
	var nlen [1]byte
	if _, err := io.ReadFull(r, nlen[:]); err != nil {
		return nil, err
	}
	nameBuf := make([]byte, nlen[0])
	if _, err := io.ReadFull(r, nameBuf); err != nil {
		return nil, err
	}
	tableName := string(nameBuf)

	var ncols [1]byte
	if _, err := io.ReadFull(r, ncols[:]); err != nil {
		return nil, err
	}
	cols := make([]ColumnDef, ncols[0])
	for i := range cols {
		var cnlen [1]byte
		if _, err := io.ReadFull(r, cnlen[:]); err != nil {
			return nil, err
		}
		cn := make([]byte, cnlen[0])
		if _, err := io.ReadFull(r, cn); err != nil {
			return nil, err
		}
		var ct [1]byte
		if _, err := io.ReadFull(r, ct[:]); err != nil {
			return nil, err
		}
		cols[i] = ColumnDef{Name: string(cn), Type: DataType(ct[0])}
	}

	// Read indexes (Phase 9 addition)
	var nidx [1]byte
	if _, err := io.ReadFull(r, nidx[:]); err != nil {
		return nil, fmt.Errorf("read index count: %w", err)
	}
	idxs := make([]IndexDef, nidx[0])
	for i := range idxs {
		var inLen [1]byte
		if _, err := io.ReadFull(r, inLen[:]); err != nil {
			return nil, err
		}
		inBuf := make([]byte, inLen[0])
		if _, err := io.ReadFull(r, inBuf); err != nil {
			return nil, err
		}
		var colLen [1]byte
		if _, err := io.ReadFull(r, colLen[:]); err != nil {
			return nil, err
		}
		colBuf := make([]byte, colLen[0])
		if _, err := io.ReadFull(r, colBuf); err != nil {
			return nil, err
		}
		idxs[i] = IndexDef{
			Name:   string(inBuf),
			Table:  strings.ToLower(tableName),
			Column: string(colBuf),
		}
	}

	return &Table{Name: tableName, Columns: cols, Indexes: idxs}, nil
}
