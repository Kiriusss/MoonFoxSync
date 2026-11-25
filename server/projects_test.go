package server

import "testing"

func TestProjectSchemaUpgrade(t *testing.T) {
	dir := t.TempDir()
	if e := Init(dir); e != nil {
		t.Fatal(e)
	}
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	id, e := s.AddUnit("existing")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("DROP VIEW effective_grants; DROP TABLE projects; PRAGMA user_version=2"); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var name string
	var version int
	if e = s.db.QueryRow("SELECT name FROM units WHERE id=?", id).Scan(&name); e != nil || name != "existing" {
		t.Fatal(name, e)
	}
	if e = s.db.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 8 {
		t.Fatal(version, e)
	}
	if _, e = s.db.Exec("SELECT unit FROM effective_grants"); e != nil {
		t.Fatal(e)
	}
}
