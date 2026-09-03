package migrations

import "embed"

// FS contains the checked-in SQL migrations compiled into migration binaries.
// The SQL files in this package are the authoritative migration source.
//
//go:embed 00001_identity_sessions.sql 00002_entitlements_devices.sql 00003_connector_oauth.sql 00004_audit_usage.sql
var FS embed.FS
