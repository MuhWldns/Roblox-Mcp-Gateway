-- +goose Up
CREATE TABLE subscriptions (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    status VARCHAR(32) NOT NULL,
    provider VARCHAR(64) NULL,
    provider_reference VARCHAR(255) NULL,
    starts_at TIMESTAMP(6) NOT NULL,
    ends_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_subscriptions_id_user (id, user_id), KEY ix_subscriptions_user (user_id),
    CONSTRAINT fk_subscriptions_user FOREIGN KEY (user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE licenses (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    roblox_identity_id CHAR(36) NULL,
    subscription_id CHAR(36) NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    device_slots INT UNSIGNED NOT NULL DEFAULT 1,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_licenses_id_user (id, user_id), KEY ix_licenses_user (user_id), KEY ix_licenses_identity (roblox_identity_id),
    CONSTRAINT fk_licenses_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_licenses_identity_owner FOREIGN KEY (roblox_identity_id, user_id) REFERENCES user_identities(id, user_id),
    CONSTRAINT fk_licenses_subscription_owner FOREIGN KEY (subscription_id, user_id) REFERENCES subscriptions(id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE trial_entitlements (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    started_at TIMESTAMP(6) NOT NULL,
    ends_at TIMESTAMP(6) NOT NULL,
    extension_reason VARCHAR(500) NULL,
    extended_by CHAR(36) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_trial_entitlements_user (user_id), UNIQUE KEY uq_trial_entitlements_id_user (id, user_id),
    CONSTRAINT fk_trial_entitlements_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_trial_entitlements_extended_by FOREIGN KEY (extended_by) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE trial_entitlement_identities (
    id CHAR(36) NOT NULL,
    trial_entitlement_id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    provider VARCHAR(64) NOT NULL,
    provider_subject VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_trial_identity_provider_subject (provider, provider_subject),
    UNIQUE KEY uq_trial_identity_id_entitlement (id, trial_entitlement_id),
    KEY ix_trial_identity_entitlement (trial_entitlement_id),
    CONSTRAINT fk_trial_identity_entitlement_owner FOREIGN KEY (trial_entitlement_id, user_id) REFERENCES trial_entitlements(id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE devices (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    name VARCHAR(255) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_devices_id_user (id, user_id), KEY ix_devices_user (user_id),
    CONSTRAINT fk_devices_user FOREIGN KEY (user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE device_enrollment_codes (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    device_id CHAR(36) NULL,
    code_digest BINARY(32) NOT NULL,
    expires_at TIMESTAMP(6) NOT NULL,
    consumed_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_enrollment_code_digest (code_digest),
    CONSTRAINT fk_enrollment_codes_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_enrollment_codes_device_owner FOREIGN KEY (device_id, user_id) REFERENCES devices(id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE device_credentials (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    device_id CHAR(36) NOT NULL,
    credential_digest BINARY(32) NOT NULL,
    expires_at TIMESTAMP(6) NULL,
    revoked_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_device_credentials_digest (credential_digest),
    CONSTRAINT fk_device_credentials_device_owner FOREIGN KEY (device_id, user_id) REFERENCES devices(id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE license_device_bindings (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    license_id CHAR(36) NOT NULL,
    device_id CHAR(36) NOT NULL,
    slot_ordinal INT UNSIGNED NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    replaced_by CHAR(36) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    revoked_at TIMESTAMP(6) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_binding_id_user (id, user_id),
    UNIQUE KEY uq_binding_license_device (license_id, device_id),
    UNIQUE KEY uq_binding_license_slot (license_id, slot_ordinal),
    KEY ix_binding_device (device_id),
    CONSTRAINT fk_binding_license_owner FOREIGN KEY (license_id, user_id) REFERENCES licenses(id, user_id),
    CONSTRAINT fk_binding_device_owner FOREIGN KEY (device_id, user_id) REFERENCES devices(id, user_id),
    CONSTRAINT fk_binding_replacement_owner FOREIGN KEY (replaced_by, user_id) REFERENCES license_device_bindings(id, user_id),
    CONSTRAINT chk_binding_slot_positive CHECK (slot_ordinal > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE bridge_connections (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    device_id CHAR(36) NOT NULL,
    connected_at TIMESTAMP(6) NOT NULL,
    disconnected_at TIMESTAMP(6) NULL,
    disconnect_reason VARCHAR(255) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), KEY ix_bridge_connections_device (device_id),
    CONSTRAINT fk_bridge_connections_device_owner FOREIGN KEY (device_id, user_id) REFERENCES devices(id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE studio_sessions (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    device_id CHAR(36) NOT NULL,
    studio_id VARCHAR(255) NOT NULL,
    status VARCHAR(32) NOT NULL,
    started_at TIMESTAMP(6) NOT NULL,
    ended_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_studio_sessions_id_user (id, user_id), KEY ix_studio_sessions_device (device_id),
    CONSTRAINT fk_studio_sessions_device_owner FOREIGN KEY (device_id, user_id) REFERENCES devices(id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE license_transfer_requests (
    id CHAR(36) NOT NULL, user_id CHAR(36) NOT NULL, license_id CHAR(36) NOT NULL, old_device_id CHAR(36) NOT NULL,
    new_device_id CHAR(36) NOT NULL, status VARCHAR(32) NOT NULL, reason VARCHAR(500) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), resolved_at TIMESTAMP(6) NULL,
    PRIMARY KEY (id), CONSTRAINT fk_transfer_license_owner FOREIGN KEY (license_id, user_id) REFERENCES licenses(id, user_id),
    CONSTRAINT fk_transfer_old_device_owner FOREIGN KEY (old_device_id, user_id) REFERENCES devices(id, user_id),
    CONSTRAINT fk_transfer_new_device_owner FOREIGN KEY (new_device_id, user_id) REFERENCES devices(id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE account_recovery_cases (
    id CHAR(36) NOT NULL, user_id CHAR(36) NOT NULL, status VARCHAR(32) NOT NULL,
    reason VARCHAR(500) NOT NULL, evidence_ref VARCHAR(500) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), resolved_at TIMESTAMP(6) NULL,
    PRIMARY KEY (id), CONSTRAINT fk_recovery_user FOREIGN KEY (user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose StatementBegin
CREATE TRIGGER trial_entitlements_no_update BEFORE UPDATE ON trial_entitlements FOR EACH ROW
BEGIN
    IF NOT (NEW.id <=> OLD.id)
        OR NOT (NEW.user_id <=> OLD.user_id)
        OR NOT (NEW.started_at <=> OLD.started_at)
        OR NOT (NEW.created_at <=> OLD.created_at)
        OR NOT (NEW.updated_at <=> OLD.updated_at)
        OR NOT (NEW.ends_at > OLD.ends_at) THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'trial entitlement updates require a later expiry and preserve immutable fields';
    END IF;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER trial_entitlements_no_delete BEFORE DELETE ON trial_entitlements FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'trial_entitlements is append-only';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER trial_entitlement_identities_no_update BEFORE UPDATE ON trial_entitlement_identities FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'trial_entitlement_identities is append-only';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER trial_entitlement_identities_no_delete BEFORE DELETE ON trial_entitlement_identities FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'trial_entitlement_identities is append-only';
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS trial_entitlement_identities_no_delete;
DROP TRIGGER IF EXISTS trial_entitlement_identities_no_update;
DROP TRIGGER IF EXISTS trial_entitlements_no_delete;
DROP TRIGGER IF EXISTS trial_entitlements_no_update;
DROP TABLE IF EXISTS account_recovery_cases;
DROP TABLE IF EXISTS license_transfer_requests;
DROP TABLE IF EXISTS studio_sessions;
DROP TABLE IF EXISTS bridge_connections;
DROP TABLE IF EXISTS license_device_bindings;
DROP TABLE IF EXISTS device_credentials;
DROP TABLE IF EXISTS device_enrollment_codes;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS trial_entitlement_identities;
DROP TABLE IF EXISTS trial_entitlements;
DROP TABLE IF EXISTS licenses;
DROP TABLE IF EXISTS subscriptions;
