package schemadiff

import "testing"

func TestParseColumns(t *testing.T) {
	ddl := []byte("CREATE TABLE `users` (\n" +
		"  `id` int NOT NULL AUTO_INCREMENT,\n" +
		"  `email` varchar(255) NOT NULL,\n" +
		"  `note` text,\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  UNIQUE KEY `email` (`email`)\n" +
		") ENGINE=InnoDB;")
	cols := ParseColumns(ddl)
	if len(cols) != 3 {
		t.Fatalf("want 3 columns, got %d: %+v", len(cols), cols)
	}
	if cols[0].Name != "id" || cols[1].Name != "email" || cols[2].Name != "note" {
		t.Fatalf("column names wrong: %+v", cols)
	}
	if cols[1].Definition != "varchar(255) NOT NULL" {
		t.Fatalf("email def wrong: %q", cols[1].Definition)
	}
}

func TestParseColumnsHandlesCommaInType(t *testing.T) {
	ddl := []byte("CREATE TABLE `t` (`amount` decimal(10,2) NOT NULL, `id` int);")
	cols := ParseColumns(ddl)
	if len(cols) != 2 {
		t.Fatalf("want 2 columns, got %d: %+v", len(cols), cols)
	}
	if cols[0].Definition != "decimal(10,2) NOT NULL" {
		t.Fatalf("decimal def split on inner comma: %q", cols[0].Definition)
	}
}

func TestDiff(t *testing.T) {
	a := []Table{
		{Name: "users", Columns: []Column{{Name: "id", Definition: "int"}, {Name: "name", Definition: "varchar(50)"}}},
		{Name: "old_table", Columns: []Column{{Name: "x", Definition: "int"}}},
	}
	b := []Table{
		{Name: "users", Columns: []Column{{Name: "id", Definition: "int"}, {Name: "name", Definition: "varchar(100)"}, {Name: "email", Definition: "varchar(255)"}}},
		{Name: "new_table", Columns: []Column{{Name: "y", Definition: "int"}}},
	}
	res := Diff(a, b)
	if len(res.AddedTables) != 1 || res.AddedTables[0] != "new_table" {
		t.Fatalf("added tables wrong: %+v", res.AddedTables)
	}
	if len(res.RemovedTables) != 1 || res.RemovedTables[0] != "old_table" {
		t.Fatalf("removed tables wrong: %+v", res.RemovedTables)
	}
	if len(res.ChangedTables) != 1 {
		t.Fatalf("want 1 changed table, got %d: %+v", len(res.ChangedTables), res.ChangedTables)
	}
	ct := res.ChangedTables[0]
	if len(ct.AddedColumns) != 1 || ct.AddedColumns[0].Name != "email" {
		t.Fatalf("added columns wrong: %+v", ct.AddedColumns)
	}
	if len(ct.ChangedColumns) != 1 || ct.ChangedColumns[0].Name != "name" {
		t.Fatalf("changed columns wrong: %+v", ct.ChangedColumns)
	}
	if ct.ChangedColumns[0].Old != "varchar(50)" || ct.ChangedColumns[0].New != "varchar(100)" {
		t.Fatalf("change detail wrong: %+v", ct.ChangedColumns[0])
	}
}
