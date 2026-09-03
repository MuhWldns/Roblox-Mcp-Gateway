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
	if _, err := Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("re-apply migrations: %v", err)
	}
	assertUniqueIndex(t, db, "trial_entitlements", "user_id")
	assertUniqueIndex(t, db, "trial_entitlement_identities", "provider", "provider_subject")
	assertForeignKey(t, db, "license_device_bindings", "device_id")
	assertCompositeForeignKey(t, db, "licenses", "roblox_identity_id", "user_id")
	assertCompositeForeignKey(t, db, "licenses", "subscription_id", "user_id")
	assertCompositeForeignKey(t, db, "license_device_bindings", "device_id", "user_id")
	assertCompositeForeignKey(t, db, "license_device_bindings", "replaced_by", "user_id")
	assertCompositeForeignKey(t, db, "device_credentials", "device_id", "user_id")
	assertCompositeForeignKey(t, db, "device_enrollment_codes", "device_id", "user_id")
	assertCompositeForeignKey(t, db, "usage_records", "device_id", "user_id")
	assertCompositeForeignKey(t, db, "usage_records", "studio_session_id", "user_id")
	assertCompositeForeignKey(t, db, "oauth_refresh_tokens", "parent_id", "user_id")
	assertTrigger(t, db, "trial_entitlements_no_update")
	assertTrigger(t, db, "trial_entitlements_no_delete")
	assertTrigger(t, db, "trial_entitlement_identities_no_update")
	assertTrigger(t, db, "trial_entitlement_identities_no_delete")
	assertTrialTablesAppendOnly(t, db)
	assertCrossTenantBindingRejected(t, db)
	assertCrossTenantLineageRejected(t, db)
	version, err := Migrate(ctx, db, "version")
	if err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version != 4 {
		t.Fatalf("migration version = %d, want 4", version)
	}
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
func assertCompositeForeignKey(t *testing.T, db *sql.DB, table, column, ownerColumn string) {
	t.Helper()
	var count int
	err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM information_schema.key_column_usage a JOIN information_schema.key_column_usage b ON a.constraint_schema = b.constraint_schema AND a.table_name = b.table_name AND a.constraint_name = b.constraint_name WHERE a.constraint_schema = DATABASE() AND a.table_name = ? AND a.column_name = ? AND b.column_name = ? AND a.referenced_table_name IS NOT NULL`, table, column, ownerColumn).Scan(&count)
	if err != nil {
		t.Fatalf("inspect composite foreign key %s.%s: %v", table, column, err)
	}
	if count == 0 {
		t.Fatalf("%s.%s lacks ownership foreign key with %s", table, column, ownerColumn)
	}
}

func assertTrigger(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema = DATABASE() AND trigger_name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("inspect trigger %s: %v", name, err)
	}
	if count != 1 {
		t.Fatalf("trigger %s count = %d, want 1", name, count)
	}
}

func assertTrialTablesAppendOnly(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `INSERT INTO users (id) VALUES ('00000000-0000-0000-0000-000000000001')`)
	if err != nil {
		t.Fatalf("insert trial test user: %v", err)
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES ('00000000-0000-0000-0000-000000000011', '00000000-0000-0000-0000-000000000001', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6))`)
	if err != nil {
		t.Fatalf("insert trial entitlement: %v", err)
	}
	_, err = db.ExecContext(t.Context(), `UPDATE trial_entitlements SET started_at = DATE_ADD(started_at, INTERVAL 1 DAY) WHERE id = '00000000-0000-0000-0000-000000000011'`)
	if err == nil {
		t.Fatal("trial entitlement started_at update unexpectedly succeeded")
	}
	if _, err = db.ExecContext(t.Context(), `UPDATE trial_entitlements SET ends_at = DATE_ADD(ends_at, INTERVAL 1 DAY), extension_reason = 'admin extension', extended_by = user_id WHERE id = '00000000-0000-0000-0000-000000000011'`); err != nil {
		t.Fatalf("trial entitlement ends_at extension failed: %v", err)
	}
	if _, err = db.ExecContext(t.Context(), `DELETE FROM trial_entitlements WHERE id = '00000000-0000-0000-0000-000000000011'`); err == nil {
		t.Fatal("trial entitlement delete unexpectedly succeeded")
	}
}

func assertCrossTenantBindingRejected(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `INSERT INTO users (id) VALUES ('00000000-0000-0000-0000-000000000002')`)
	if err != nil {
		t.Fatalf("insert second test user: %v", err)
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO devices (id, user_id, name) VALUES ('00000000-0000-0000-0000-000000000022', '00000000-0000-0000-0000-000000000002', 'other')`)
	if err != nil {
		t.Fatalf("insert second device: %v", err)
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO licenses (id, user_id, status) VALUES ('00000000-0000-0000-0000-000000000033', '00000000-0000-0000-0000-000000000001', 'active')`)
	if err != nil {
		t.Fatalf("insert test license: %v", err)
	}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal) VALUES ('00000000-0000-0000-0000-000000000044', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000033', '00000000-0000-0000-0000-000000000022', 1)`); err == nil {
		t.Fatal("cross-tenant binding unexpectedly succeeded")
	}
}
func assertCrossTenantLineageRejected(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, replaced_by) VALUES ('00000000-0000-0000-0000-000000000055', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000033', '00000000-0000-0000-0000-000000000001', 2, '00000000-0000-0000-0000-000000000044')`); err == nil {
		t.Fatal("cross-tenant binding replacement unexpectedly succeeded")
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO oauth_grants (id, user_id, client_id, scopes, resource) VALUES ('00000000-0000-0000-0000-000000000066', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000067', JSON_ARRAY(), '')`); err == nil {
		t.Fatal("invalid OAuth grant unexpectedly succeeded")
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
