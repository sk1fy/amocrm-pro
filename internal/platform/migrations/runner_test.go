package migrations

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRequiresStrictPairedNames(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir, "000001-init.up.sql", "SELECT 1")
	writeMigrationFile(t, dir, "000001-init.down.sql", "SELECT 1")
	_, err := (&Runner{dir: dir}).load()
	if err == nil {
		t.Fatal("expected invalid migration name to fail")
	}
}

func TestLoadRequiresDownPair(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir, "000001_init.up.sql", "SELECT 1")
	_, err := (&Runner{dir: dir}).load()
	if err == nil {
		t.Fatal("expected missing down migration to fail")
	}
}

func TestLoadHashesBothDirections(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir, "000001_init.up.sql", "SELECT 1")
	writeMigrationFile(t, dir, "000001_init.down.sql", "SELECT 2")
	loaded, err := (&Runner{dir: dir}).load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].checksum == loaded[0].downChecksum {
		t.Fatalf("unexpected migration hashes: %#v", loaded)
	}
}

func writeMigrationFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
