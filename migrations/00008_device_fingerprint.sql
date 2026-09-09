-- +goose Up
-- Add hardware fingerprint hash to devices table.
-- The fingerprint_hash stores a 32-byte binary HMAC-SHA256 of the stable
-- Bridge hardware identity. It is nullable solely for migration compatibility
-- with historical device rows; new enrollment clients are not permitted to omit it.
-- A UNIQUE index guarantees that the same hardware fingerprint cannot be
-- registered multiple times across accounts.
ALTER TABLE devices
    ADD COLUMN fingerprint_hash BINARY(32) NULL AFTER bridge_version,
    ADD UNIQUE KEY uq_devices_fingerprint (fingerprint_hash);

-- +goose Down
ALTER TABLE devices
    DROP KEY uq_devices_fingerprint,
    DROP COLUMN fingerprint_hash;
