package postgres

import (
	"testing"
	"testing/fstest"
)

func TestLoadMigrationsOrdersAndChecksumsFiles(t *testing.T) {
	got, err := loadMigrations(fstest.MapFS{
		"0002_two.up.sql": {Data: []byte("SELECT 2;")},
		"README.md":       {Data: []byte("ignored")},
		"0001_one.up.sql": {Data: []byte("SELECT 1;")},
	})
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(got) != 2 || got[0].Name != "0001_one.up.sql" || got[1].Name != "0002_two.up.sql" {
		t.Fatalf("loadMigrations() = %#v", got)
	}
	if checksumString(got[0].Checksum) == checksumString(got[1].Checksum) {
		t.Fatal("different migrations must not have the same checksum")
	}
}
