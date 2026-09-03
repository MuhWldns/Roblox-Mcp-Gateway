package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestMigrationsCreateTrialAndBindingConstraints(t *testing.T) {
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
	defer admin.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName := fmt.Sprintf("robloxkit_test_%d", time.Now().UnixNano())
	if !isSafeIdentifier(dbName) {
		t.Fatal("generated unsafe temporary database name")
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create temporary database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`")
	})
	target := *base
	target.DBName = dbName
	db, err := sql.Open("mysql", target.FormatDSN())
	if err != nil {
		t.Fatalf("open temporary database: %v", err)
	}
	defer db.Close()
	if err := Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if err := Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("re-apply migrations: %v", err)
	}
	assertUniqueIndex(t, db, "trial_entitlements", "user_id")
	assertUniqueIndex(t, db, "trial_entitlement_identities", "provider", "provider_subject")
	assertForeignKey(t, db, "license_device_bindings", "device_id")
	assertBinaryDigest(t, db, "web_sessions", "token_digest")
	assertNoPlaintextTokenColumns(t, db)
}

func isSafeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func assertUniqueIndex(t *testing.T, db *sql.DB, table string, columns ...string) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT index_name, non_unique, seq_in_index, column_name FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? ORDER BY index_name, seq_in_index`, table)
	if err != nil {
		t.Fatalf("inspect indexes for %s: %v", table, err)
	}
	defer rows.Close()
	got := map[string][]string{}
	unique := map[string]bool{}
	for rows.Next() {
		var index string
		var nonUnique, seq int
		var column string
		if err := rows.Scan(&index, &nonUnique, &seq, &column); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		got[index] = append(got[index], column)
		unique[index] = nonUnique == 0
	}
	for index, gotColumns := range got {
		if unique[index] && equalStrings(gotColumns, columns) {
			return
		}
	}
	t.Fatalf("%s lacks unique index on (%s)", table, strings.Join(columns, ", "))
}

func assertForeignKey(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM information_schema.key_column_usage WHERE constraint_schema = DATABASE() AND table_name = ? AND column_name = ? AND referenced_table_name IS NOT NULL`, table, column).Scan(&count); err != nil {
		t.Fatalf("inspect foreign key %s.%s: %v", table, column, err)
	}
	if count == 0 {
		t.Fatalf("%s.%s has no foreign key", table, column)
	}
}

func assertBinaryDigest(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	var dataType string
	if err := db.QueryRowContext(t.Context(), `SELECT data_type FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`, table, column).Scan(&dataType); err != nil {
		t.Fatalf("inspect digest column: %v", err)
	}
	if dataType != "binary" {
		t.Fatalf("%s.%s data type = %q, want binary", table, column, dataType)
	}
}

func assertNoPlaintextTokenColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND (LOWER(column_name) LIKE '%token%' OR LOWER(column_name) LIKE '%secret%' OR LOWER(column_name) LIKE '%password%')`)
	if err != nil {
		t.Fatalf("inspect secret columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatalf("scan secret column: %v", err)
		}
		var dataType string
		if err := db.QueryRowContext(t.Context(), `SELECT data_type FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`, table, column).Scan(&dataType); err != nil {
			t.Fatalf("inspect secret data type: %v", err)
		}
		if dataType != "binary" && column != "family_id" && column != "parent_id" {
			t.Fatalf("possible plaintext secret column %s.%s (%s)", table, column, dataType)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
