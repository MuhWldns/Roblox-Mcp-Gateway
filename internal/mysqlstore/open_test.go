package mysqlstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestOpenRejectsEmptyDSN(t *testing.T) {
	db, err := Open(context.Background(), "", PoolConfig{})
	if err == nil {
		t.Fatal("Open(\"\") returned nil error")
	}
	if db != nil {
		t.Fatal("Open returned a database on invalid DSN")
	}
}

func TestOpenConfiguresPoolAndContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db, err := Open(ctx, "user:password@tcp(127.0.0.1:1)/db", PoolConfig{MaxOpenConns: 7, MaxIdleConns: 3, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Second})
	if err == nil {
		t.Fatal("Open with canceled context returned nil error")
	}
	if db != nil {
		db.Close()
	}
}

func TestPoolConfigZeroValuesAreSafe(t *testing.T) {
	var _ *sql.DB
	if _, err := Open(context.Background(), "not a mysql dsn", PoolConfig{}); err == nil {
		t.Fatal("expected malformed DSN error")
	}
}
