package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// PoolConfig controls the database connection pool and driver timeouts.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	ConnectTimeout  time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
}

// Open creates and verifies a MySQL connection pool. It never performs schema
// migrations; callers must run the migrate command before accepting traffic.
func Open(ctx context.Context, dsn string, cfg PoolConfig) (*sql.DB, error) {
	if ctx == nil {
		return nil, errors.New("mysqlstore: nil context")
	}
	if dsn == "" {
		return nil, errors.New("mysqlstore: empty DSN")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: parse DSN: %w", err)
	}
	parsed.ParseTime = true
	parsed.Loc = time.UTC
	if cfg.ConnectTimeout > 0 {
		parsed.Timeout = cfg.ConnectTimeout
	} else if parsed.Timeout == 0 {
		parsed.Timeout = 5 * time.Second
	}
	if cfg.ReadTimeout > 0 {
		parsed.ReadTimeout = cfg.ReadTimeout
	} else if parsed.ReadTimeout == 0 {
		parsed.ReadTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout > 0 {
		parsed.WriteTimeout = cfg.WriteTimeout
	} else if parsed.WriteTimeout == 0 {
		parsed.WriteTimeout = 10 * time.Second
	}

	db, err := sql.Open("mysql", parsed.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: open: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("mysqlstore: ping: %w", err)
	}
	return db, nil
}
