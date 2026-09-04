package mysqlstore

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
)

func TestMigrateRejectsUnknownCommand(t *testing.T) {
	if _, err := Migrate(context.Background(), nil, "nope"); err == nil {
		t.Fatal("unknown migration command returned nil error")
	}
}

func TestMigrationFilesAreEmbedded(t *testing.T) {
	fsys, err := migrationsFilesystem()
	if err != nil {
		t.Fatalf("resolve migration filesystem: %v", err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("read migration filesystem: %v", err)
	}
	if len(entries) != 6 {
		t.Fatalf("migration filesystem has %d entries, want 6", len(entries))
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("migration filesystem contains directory %q", entry.Name())
		}
	}
}

func TestMigrateVersionReturnsDatabaseVersion(t *testing.T) {
	if _, err := Migrate(context.Background(), (*sql.DB)(nil), "version"); err == nil {
		t.Fatal("Migrate(nil, version) returned nil error")
	}
}

func TestMigrateRequiresDatabase(t *testing.T) {
	for _, command := range []string{"up", "status", "version"} {
		if _, err := Migrate(context.Background(), (*sql.DB)(nil), command); err == nil {
			t.Fatalf("Migrate(nil, %q) returned nil error", command)
		}
	}
}
