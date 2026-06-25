// Package catalog manages table schemas for the database.
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

const catalogMagic = uint32(0xCA7A1060)

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

// Table is the schema for one relation.
type Table struct {
	Name    string
	Columns []ColumnDef
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

// Catalog is an in-memory map of all table schemas, backed by a binary file.
type Catalog struct {
	tables map[string]*Table // keyed by lowercase table name
	path   string
}

// New returns an empty Catalog backed by path.
// Call Save() after mutations to persist changes.
func New(path string) *Catalog {
	return &Catalog{tables: make(map[string]*Table), path: path}
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
// We overwrite the file in place (Phase 4 will add WAL protection here).
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

// --- binary serialization ---
//
// File format:
//   [0–3]  magic uint32 = 0xCA7A1060
//   [4–5]  numTables uint16
//   For each table:
//     nameLen uint8  + name bytes
//     numCols uint8
//     For each column:
//       colNameLen uint8 + colName bytes
//       colType    uint8

func (c *Catalog) serialize(w io.Writer) error {
	var magic [4]byte
	binary.LittleEndian.PutUint32(magic[:], catalogMagic)
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
	if binary.LittleEndian.Uint32(magic[:]) != catalogMagic {
		return fmt.Errorf("bad magic number — file is not a catalog")
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
		c.tables[strings.ToLower(t.Name)] = t
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
	return &Table{Name: string(nameBuf), Columns: cols}, nil
}
