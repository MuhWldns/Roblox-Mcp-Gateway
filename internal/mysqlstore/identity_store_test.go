package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"robloxkit/internal/robloxauth"
)

func TestIdentityStoreKeysOnlyBySubjectAndUpdatesMutableDisplayName(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewIdentityStore(db)

	first, err := store.UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{Subject: "1516563360", Username: "Builderman", DisplayName: "Builder Man"})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.DisplayName != "Builder Man" {
		t.Fatalf("insert stored username instead of display name: %#v", first)
	}
	changed, err := store.UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{Subject: "1516563360", Username: "NewBuilderman", DisplayName: "Builder Renamed"})
	if err != nil {
		t.Fatalf("metadata upsert: %v", err)
	}
	if changed.ID != first.ID || changed.IdentityID != first.IdentityID {
		t.Fatalf("metadata update remapped account: first=%#v changed=%#v", first, changed)
	}
	if changed.DisplayName != "Builder Renamed" || changed.RobloxSubject != "1516563360" {
		t.Fatalf("updated user = %#v", changed)
	}

	other, err := store.UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{Subject: "999", Username: "Builderman", DisplayName: "Builder Renamed"})
	if err != nil {
		t.Fatalf("same metadata with another subject: %v", err)
	}
	if other.ID == first.ID || other.IdentityID == first.IdentityID {
		t.Fatalf("mutable metadata merged distinct subjects: first=%#v other=%#v", first, other)
	}

	var users, identities int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM user_identities").Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if users != 2 || identities != 2 {
		t.Fatalf("rows users=%d identities=%d, want 2/2", users, identities)
	}
}

func TestIdentityStorePreservesCaseAndAccentDistinctSubjects(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewIdentityStore(db)

	inputs := []string{"subject", "SUBJECT", "cafe", "café"}
	users := make(map[string]struct{}, len(inputs))
	identities := make(map[string]struct{}, len(inputs))
	for _, subject := range inputs {
		user, err := store.UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{Subject: subject, DisplayName: subject})
		if err != nil {
			t.Fatalf("upsert subject %q: %v", subject, err)
		}
		users[user.ID] = struct{}{}
		identities[user.IdentityID] = struct{}{}
	}
	if len(users) != len(inputs) || len(identities) != len(inputs) {
		t.Fatalf("exact subjects collapsed: users=%d identities=%d, want %d each", len(users), len(identities), len(inputs))
	}
}

func TestIdentityStoreConcurrentCollisionReturnsOneStableUser(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewIdentityStore(db)
	const workers = 12
	start := make(chan struct{})
	results := make(chan robloxauth.User, workers)
	errors := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for i := range workers {
		go func() {
			ready.Done()
			<-start
			user, err := store.UpsertRobloxIdentity(context.Background(), robloxauth.RobloxIdentity{
				Subject: "1516563360", Username: fmt.Sprintf("Builder-%d", i), DisplayName: fmt.Sprintf("Builder %d", i),
			})
			results <- user
			errors <- err
		}()
	}
	ready.Wait()
	close(start)

	var want robloxauth.User
	for range workers {
		user := <-results
		if err := <-errors; err != nil {
			t.Fatalf("concurrent upsert: %v", err)
		}
		if want.ID == "" {
			want = user
		}
		if user.ID != want.ID || user.IdentityID != want.IdentityID {
			t.Fatalf("collision returned multiple mappings: want=%#v got=%#v", want, user)
		}
	}
	var users, identities int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM user_identities").Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if users != 1 || identities != 1 {
		t.Fatalf("collision left duplicate rows users=%d identities=%d", users, identities)
	}
}

func TestIdentityStoreRejectsMissingSubject(t *testing.T) {
	store := NewIdentityStore(nil)
	if _, err := store.UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{}); err == nil {
		t.Fatal("missing subject was accepted")
	}
}

func identityTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
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
	t.Cleanup(cancel)
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName := fmt.Sprintf("robloxkit_identity_test_%d", time.Now().UnixNano())
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
	return db
}
