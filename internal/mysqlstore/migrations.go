package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pressly/goose/v3"
)

// migrationFS points at the checked-in migrations directory for inspection by
// tests and tooling. It is resolved lazily because package tests run with the
// package directory as their working directory.
var migrationFS fs.FS

func migrationsFilesystem() (fs.FS, error) {
	if migrationFS != nil {
		return migrationFS, nil
	}
	candidates := make([]string, 0, 6)
	if dir := os.Getenv("ROBLOXKIT_MIGRATIONS_DIR"); dir != "" {
		candidates = append(candidates, dir)
	}
	if dir, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "migrations"), filepath.Join(dir, "..", "..", "migrations"))
	}
	if executable, err := os.Executable(); err == nil {
		executableDir := filepath.Dir(executable)
		candidates = append(candidates, filepath.Join(executableDir, "migrations"), filepath.Join(executableDir, "..", "migrations"))
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "..", "..", "migrations"))
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "00001_identity_sessions.sql")); err == nil {
			migrationFS = os.DirFS(dir)
			return migrationFS, nil
		}
	}
	return nil, errors.New("mysqlstore: migrations directory not found")
}

// Migrate runs an explicit migration command. Normal server startup must not
// call this function; schema changes are a deployment operation.
func Migrate(ctx context.Context, db *sql.DB, command string) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if db == nil {
		return errors.New("mysqlstore: nil database")
	}
	return migrateDB(ctx, db, command)
}

// migrateDB is kept separate so the public contract can remain strict while
// tests and callers receive a useful error before any migration work.
func migrateDB(ctx context.Context, db *sql.DB, command string) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if db == nil {
		return errors.New("mysqlstore: nil database")
	}
	if command != "up" && command != "status" && command != "version" {
		return fmt.Errorf("mysqlstore: unsupported migration command %q", command)
	}
	fsys, err := migrationsFilesystem()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, db, fsys)
	if err != nil {
		return fmt.Errorf("mysqlstore: create migration provider: %w", err)
	}
	switch command {
	case "up":
		_, err = provider.Up(ctx)
	case "status":
		_, err = provider.Status(ctx)
	case "version":
		_, err = provider.GetDBVersion(ctx)
	}
	if err != nil {
		return fmt.Errorf("mysqlstore: %s migrations: %w", command, err)
	}
	return nil
}
