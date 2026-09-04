package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"robloxkit/internal/credential"
	"robloxkit/internal/session"
)

func TestSessionStoreUsesDigestOnlyQueries(t *testing.T) {
	rawDSN := os.Getenv("MYSQL_TEST_DSN")
	if rawDSN == "" {
		t.Skip("MYSQL_TEST_DSN is not configured")
	}
	base, err := mysql.ParseDSN(rawDSN)
	if err != nil {
		t.Fatalf("parse MYSQL_TEST_DSN: %v", err)
	}
	adminConfig := *base
	adminConfig.DBName = ""
	admin, err := sql.Open("mysql", adminConfig.FormatDSN())
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin database: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName := fmt.Sprintf("robloxkit_session_test_%d", time.Now().UnixNano())
	if !isSafeIdentifier(dbName) {
		t.Fatal("generated unsafe temporary database name")
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create temporary database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`"); err != nil {
			t.Errorf("drop temporary database: %v", err)
		}
	})
	target := *base
	target.DBName = dbName
	target.ParseTime = true
	target.Loc = time.UTC
	db, err := sql.Open("mysql", target.FormatDSN())
	if err != nil {
		t.Fatalf("open temporary database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close temporary database: %v", err)
		}
	})
	if _, err := Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO users (id) VALUES (?)", "user-session-test"); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	plain, digest, err := credential.Generate("rks_", 32, []byte("test-pepper"))
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	created := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	stored := session.Session{ID: "session-test", UserID: "user-session-test", CreatedAt: created, LastSeenAt: created, ExpiresAt: created.Add(time.Hour)}
	store := NewSessionStore(db)
	if err := store.Insert(ctx, stored, digest); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	var persisted []byte
	if err := db.QueryRowContext(ctx, "SELECT token_digest FROM web_sessions WHERE id = ?", stored.ID).Scan(&persisted); err != nil {
		t.Fatalf("read digest: %v", err)
	}
	if len(persisted) != 32 || string(persisted) != string(digest[:]) {
		t.Fatalf("persisted token_digest is not the keyed digest: %x", persisted)
	}
	if strings.Contains(string(persisted), plain) {
		t.Fatal("plaintext token was persisted")
	}
	var plaintextColumns int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'web_sessions' AND column_name LIKE '%token%' AND data_type NOT IN ('binary', 'varbinary')`).Scan(&plaintextColumns); err != nil {
		t.Fatalf("inspect token columns: %v", err)
	}
	if plaintextColumns != 0 {
		t.Fatalf("web_sessions contains %d non-binary token columns", plaintextColumns)
	}
	got, err := store.ValidateAndTouch(ctx, digest, created)
	if err != nil || got.ID != stored.ID || got.UserID != stored.UserID {
		t.Fatalf("validate session = %#v, err=%v", got, err)
	}
	web := &session.Service{Store: store, Pepper: []byte("test-pepper"), Lifetime: time.Hour, Now: func() time.Time { return created }}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, err := web.Rotate(ctx, plain); results <- err }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent MySQL rotations succeeded %d times, want 1", successes)
	}
}
