package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/pressly/goose/v3"
	"io/fs"

	migrationfiles "robloxkit/migrations"
)

var migrationFS fs.FS = migrationfiles.FS

func migrationsFilesystem() (fs.FS, error) {
	if migrationFS == nil {
		return nil, errors.New("mysqlstore: embedded migrations unavailable")
	}
	return migrationFS, nil
}

// Migrate runs an explicit migration command and returns the resulting schema
// version. Normal server startup must not call this function; schema changes
// are a deployment operation.
func Migrate(ctx context.Context, db *sql.DB, command string) (int64, error) {
	if ctx == nil {
		return 0, errors.New("mysqlstore: nil context")
	}
	if db == nil {
		return 0, errors.New("mysqlstore: nil database")
	}
	if command != "up" && command != "status" && command != "version" {
		return 0, fmt.Errorf("mysqlstore: unsupported migration command %q", command)
	}
	fsys, err := migrationsFilesystem()
	if err != nil {
		return 0, err
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, db, fsys)
	if err != nil {
		return 0, fmt.Errorf("mysqlstore: create migration provider: %w", err)
	}
	var version int64
	switch command {
	case "up":
		_, err = provider.Up(ctx)
		if err == nil {
			version, err = provider.GetDBVersion(ctx)
		}
	case "status":
		_, err = provider.Status(ctx)
		if err == nil {
			version, err = provider.GetDBVersion(ctx)
		}
	case "version":
		version, err = provider.GetDBVersion(ctx)
	}
	if err != nil {
		return 0, fmt.Errorf("mysqlstore: %s migrations: %w", command, err)
	}
	return version, nil
}
