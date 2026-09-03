package mysqlstore

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
)

func TestMigrateRejectsUnknownCommand(t *testing.T) {
	if err := Migrate(context.Background(), nil, "nope"); err == nil {
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
	if len(entries) == 0 {
		t.Fatal("migration filesystem is empty")
	}
}

func TestMigrateRequiresDatabase(t *testing.T) {
	for _, command := range []string{"up", "status", "version"} {
		if err := Migrate(context.Background(), (*sql.DB)(nil), command); err == nil {
			t.Fatalf("Migrate(nil, %q) returned nil error", command)
		}
	}
}
