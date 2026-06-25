package catalog

import (
	"os"
	"testing"
)

func TestNewCatalogIsEmpty(t *testing.T) {
	c := New("/dev/null/nonexistent")
	if len(c.Tables()) != 0 {
		t.Errorf("new catalog should have 0 tables, got %d", len(c.Tables()))
	}
}

func TestCreateAndGetTable(t *testing.T) {
	f, _ := os.CreateTemp("", "catalog-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	c := New(path)
	tbl := &Table{
		Name:    "users",
		Columns: []ColumnDef{{Name: "id", Type: TypeInt}, {Name: "name", Type: TypeText}},
	}
	if err := c.CreateTable(tbl); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	got, ok := c.GetTable("users")
	if !ok {
		t.Fatal("GetTable('users'): not found")
	}
	if got.Name != "users" || len(got.Columns) != 2 {
		t.Errorf("unexpected table: %+v", got)
	}
}

func TestCreateTableDuplicateName(t *testing.T) {
	f, _ := os.CreateTemp("", "catalog-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	c := New(path)
	tbl := &Table{Name: "foo", Columns: []ColumnDef{{Name: "id", Type: TypeInt}}}
	c.CreateTable(tbl)
	if err := c.CreateTable(tbl); err == nil {
		t.Error("expected error creating duplicate table, got nil")
	}
}

func TestGetTableCaseInsensitive(t *testing.T) {
	f, _ := os.CreateTemp("", "catalog-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	c := New(path)
	c.CreateTable(&Table{Name: "Users", Columns: []ColumnDef{{Name: "id", Type: TypeInt}}})
	if _, ok := c.GetTable("USERS"); !ok {
		t.Error("GetTable should be case-insensitive")
	}
}

func TestCatalogSaveAndLoad(t *testing.T) {
	f, _ := os.CreateTemp("", "catalog-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	c := New(path)
	c.CreateTable(&Table{
		Name:    "products",
		Columns: []ColumnDef{{Name: "id", Type: TypeInt}, {Name: "label", Type: TypeText}, {Name: "price", Type: TypeInt}},
	})

	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tbl, ok := c2.GetTable("products")
	if !ok {
		t.Fatal("'products' not found after reload")
	}
	if len(tbl.Columns) != 3 {
		t.Errorf("columns: got %d, want 3", len(tbl.Columns))
	}
	if tbl.Columns[0].Name != "id" || tbl.Columns[0].Type != TypeInt {
		t.Errorf("column 0: got %+v", tbl.Columns[0])
	}
	if tbl.Columns[1].Name != "label" || tbl.Columns[1].Type != TypeText {
		t.Errorf("column 1: got %+v", tbl.Columns[1])
	}
	if tbl.Columns[2].Name != "price" || tbl.Columns[2].Type != TypeInt {
		t.Errorf("column 2: got %+v", tbl.Columns[2])
	}
}

func TestLoadNonexistentReturnsEmpty(t *testing.T) {
	c, err := Load("/tmp/definitely-does-not-exist-db-engine-test.catalog")
	if err != nil {
		t.Fatalf("Load on nonexistent file should succeed: %v", err)
	}
	if len(c.Tables()) != 0 {
		t.Error("expected empty catalog for nonexistent file")
	}
}

func TestPrimaryKeyIndex(t *testing.T) {
	tbl := &Table{
		Columns: []ColumnDef{{Name: "name", Type: TypeText}, {Name: "id", Type: TypeInt}},
	}
	if got := tbl.PrimaryKeyIndex(); got != 1 {
		t.Errorf("PrimaryKeyIndex: got %d, want 1", got)
	}
}

func TestPrimaryKeyIndexNone(t *testing.T) {
	tbl := &Table{Columns: []ColumnDef{{Name: "x", Type: TypeText}}}
	if got := tbl.PrimaryKeyIndex(); got != -1 {
		t.Errorf("PrimaryKeyIndex: got %d, want -1 for text-only table", got)
	}
}

func TestColIndex(t *testing.T) {
	tbl := &Table{
		Columns: []ColumnDef{{Name: "id", Type: TypeInt}, {Name: "name", Type: TypeText}},
	}
	if got := tbl.ColIndex("NAME"); got != 1 {
		t.Errorf("ColIndex('NAME'): got %d, want 1", got)
	}
	if got := tbl.ColIndex("missing"); got != -1 {
		t.Errorf("ColIndex('missing'): got %d, want -1", got)
	}
}
