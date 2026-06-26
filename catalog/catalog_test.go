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

// --- Phase 9: secondary index tests ---

func TestCreateIndex(t *testing.T) {
	f, _ := os.CreateTemp("", "catalog-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	c := New(path)
	c.CreateTable(&Table{Name: "users", Columns: []ColumnDef{{Name: "id", Type: TypeInt}, {Name: "age", Type: TypeInt}}})

	if err := c.CreateIndex(IndexDef{Name: "idx_users_age", Table: "users", Column: "age"}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	tbl, _ := c.GetTable("users")
	if len(tbl.Indexes) != 1 {
		t.Fatalf("expected 1 index, got %d", len(tbl.Indexes))
	}
	if tbl.Indexes[0].Name != "idx_users_age" {
		t.Errorf("index name: got %q, want %q", tbl.Indexes[0].Name, "idx_users_age")
	}
	if tbl.Indexes[0].Column != "age" {
		t.Errorf("index column: got %q, want %q", tbl.Indexes[0].Column, "age")
	}
}

func TestCreateIndexValidation(t *testing.T) {
	f, _ := os.CreateTemp("", "catalog-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	c := New(path)
	c.CreateTable(&Table{
		Name:    "users",
		Columns: []ColumnDef{{Name: "id", Type: TypeInt}, {Name: "name", Type: TypeText}, {Name: "age", Type: TypeInt}},
	})

	cases := []struct {
		label string
		def   IndexDef
	}{
		{"nonexistent table", IndexDef{Name: "idx1", Table: "missing", Column: "age"}},
		{"nonexistent column", IndexDef{Name: "idx1", Table: "users", Column: "missing"}},
		{"TEXT column", IndexDef{Name: "idx1", Table: "users", Column: "name"}},
	}
	for _, tc := range cases {
		if err := c.CreateIndex(tc.def); err == nil {
			t.Errorf("CreateIndex(%s): expected error, got nil", tc.label)
		}
	}

	// First valid index succeeds.
	if err := c.CreateIndex(IndexDef{Name: "idx_users_age", Table: "users", Column: "age"}); err != nil {
		t.Fatalf("CreateIndex (first): %v", err)
	}

	// Duplicate index name.
	if err := c.CreateIndex(IndexDef{Name: "idx_users_age", Table: "users", Column: "id"}); err == nil {
		t.Error("expected error for duplicate index name")
	}
	// Second index on same column.
	if err := c.CreateIndex(IndexDef{Name: "idx_users_age2", Table: "users", Column: "age"}); err == nil {
		t.Error("expected error for second index on same column")
	}
}

func TestDropIndex(t *testing.T) {
	f, _ := os.CreateTemp("", "catalog-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	c := New(path)
	c.CreateTable(&Table{Name: "t", Columns: []ColumnDef{{Name: "id", Type: TypeInt}, {Name: "score", Type: TypeInt}}})
	c.CreateIndex(IndexDef{Name: "idx_score", Table: "t", Column: "score"})

	if err := c.DropIndex("idx_score"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}

	tbl, _ := c.GetTable("t")
	if len(tbl.Indexes) != 0 {
		t.Errorf("expected 0 indexes after drop, got %d", len(tbl.Indexes))
	}
	if _, ok := c.GetIndex("idx_score"); ok {
		t.Error("GetIndex should return false after drop")
	}

	// Dropping a nonexistent index is an error.
	if err := c.DropIndex("idx_score"); err == nil {
		t.Error("expected error dropping nonexistent index")
	}
}

func TestCatalogPersistsIndexes(t *testing.T) {
	f, _ := os.CreateTemp("", "catalog-*.bin")
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	c := New(path)
	c.CreateTable(&Table{Name: "products", Columns: []ColumnDef{{Name: "id", Type: TypeInt}, {Name: "price", Type: TypeInt}}})
	c.CreateIndex(IndexDef{Name: "idx_price", Table: "products", Column: "price"})

	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tbl, ok := c2.GetTable("products")
	if !ok {
		t.Fatal("products table not found after reload")
	}
	if len(tbl.Indexes) != 1 {
		t.Fatalf("expected 1 index after reload, got %d", len(tbl.Indexes))
	}
	if tbl.Indexes[0].Name != "idx_price" || tbl.Indexes[0].Column != "price" {
		t.Errorf("wrong index after reload: %+v", tbl.Indexes[0])
	}

	def, ok := c2.GetIndex("idx_price")
	if !ok {
		t.Fatal("GetIndex should find index after reload")
	}
	if def.Table != "products" {
		t.Errorf("index.Table after reload: got %q, want %q", def.Table, "products")
	}
}

func TestIndexForColumn(t *testing.T) {
	tbl := &Table{
		Name:    "t",
		Columns: []ColumnDef{{Name: "id", Type: TypeInt}, {Name: "age", Type: TypeInt}},
		Indexes: []IndexDef{{Name: "idx_age", Table: "t", Column: "age"}},
	}

	if got := tbl.IndexForColumn("AGE"); got == nil || got.Name != "idx_age" {
		t.Errorf("IndexForColumn('AGE'): expected idx_age, got %v", got)
	}
	if got := tbl.IndexForColumn("id"); got != nil {
		t.Errorf("IndexForColumn('id'): expected nil (no index on id), got %v", got)
	}
}
