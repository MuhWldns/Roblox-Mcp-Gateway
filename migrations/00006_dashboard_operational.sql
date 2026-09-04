-- +goose Up
ALTER TABLE devices
    ADD COLUMN hostname VARCHAR(255) NULL AFTER name,
    ADD COLUMN platform VARCHAR(64) NULL AFTER hostname,
    ADD COLUMN bridge_version VARCHAR(64) NULL AFTER platform,
    ADD COLUMN last_heartbeat_at TIMESTAMP(6) NULL AFTER bridge_version,
    ADD COLUMN official_mcp_state VARCHAR(32) NULL AFTER last_heartbeat_at,
    ADD COLUMN reconnect_count INT UNSIGNED NOT NULL DEFAULT 0 AFTER official_mcp_state,
    ADD COLUMN last_error VARCHAR(500) NULL AFTER reconnect_count;

-- +goose Down
ALTER TABLE devices
    DROP COLUMN hostname,
    DROP COLUMN platform,
    DROP COLUMN bridge_version,
    DROP COLUMN last_heartbeat_at,
    DROP COLUMN official_mcp_state,
    DROP COLUMN reconnect_count,
    DROP COLUMN last_error;