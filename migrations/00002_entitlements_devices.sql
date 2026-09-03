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
    PRIMARY KEY (id), KEY ix_subscriptions_user (user_id),
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
    PRIMARY KEY (id), KEY ix_licenses_user (user_id), KEY ix_licenses_identity (roblox_identity_id),
    CONSTRAINT fk_licenses_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_licenses_identity FOREIGN KEY (roblox_identity_id) REFERENCES user_identities(id),
    CONSTRAINT fk_licenses_subscription FOREIGN KEY (subscription_id) REFERENCES subscriptions(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE trial_entitlements (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    started_at TIMESTAMP(6) NOT NULL,
    ends_at TIMESTAMP(6) NOT NULL,
    extension_reason VARCHAR(500) NULL,
    extended_by CHAR(36) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_trial_entitlements_user (user_id),
    CONSTRAINT fk_trial_entitlements_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_trial_entitlements_extended_by FOREIGN KEY (extended_by) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE trial_entitlement_identities (
    id CHAR(36) NOT NULL,
    trial_entitlement_id CHAR(36) NOT NULL,
    provider VARCHAR(64) NOT NULL,
    provider_subject VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_trial_identity_provider_subject (provider, provider_subject),
    KEY ix_trial_identity_entitlement (trial_entitlement_id),
    CONSTRAINT fk_trial_identity_entitlement FOREIGN KEY (trial_entitlement_id) REFERENCES trial_entitlements(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE devices (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    name VARCHAR(255) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), KEY ix_devices_user (user_id),
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
    CONSTRAINT fk_enrollment_codes_device FOREIGN KEY (device_id) REFERENCES devices(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE device_credentials (
    id CHAR(36) NOT NULL,
    device_id CHAR(36) NOT NULL,
    credential_digest BINARY(32) NOT NULL,
    expires_at TIMESTAMP(6) NULL,
    revoked_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_device_credentials_digest (credential_digest),
    CONSTRAINT fk_device_credentials_device FOREIGN KEY (device_id) REFERENCES devices(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE license_device_bindings (
    id CHAR(36) NOT NULL,
    license_id CHAR(36) NOT NULL,
    device_id CHAR(36) NOT NULL,
    slot_ordinal INT UNSIGNED NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    replaced_by CHAR(36) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    revoked_at TIMESTAMP(6) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_binding_license_device (license_id, device_id),
    UNIQUE KEY uq_binding_license_slot (license_id, slot_ordinal),
    KEY ix_binding_device (device_id),
    CONSTRAINT fk_binding_license FOREIGN KEY (license_id) REFERENCES licenses(id),
    CONSTRAINT fk_binding_device FOREIGN KEY (device_id) REFERENCES devices(id),
    CONSTRAINT fk_binding_replacement FOREIGN KEY (replaced_by) REFERENCES license_device_bindings(id),
    CONSTRAINT chk_binding_slot_positive CHECK (slot_ordinal > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE bridge_connections (
    id CHAR(36) NOT NULL,
    device_id CHAR(36) NOT NULL,
    connected_at TIMESTAMP(6) NOT NULL,
    disconnected_at TIMESTAMP(6) NULL,
    disconnect_reason VARCHAR(255) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), KEY ix_bridge_connections_device (device_id),
    CONSTRAINT fk_bridge_connections_device FOREIGN KEY (device_id) REFERENCES devices(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE studio_sessions (
    id CHAR(36) NOT NULL,
    device_id CHAR(36) NOT NULL,
    studio_id VARCHAR(255) NOT NULL,
    status VARCHAR(32) NOT NULL,
    started_at TIMESTAMP(6) NOT NULL,
    ended_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), KEY ix_studio_sessions_device (device_id),
    CONSTRAINT fk_studio_sessions_device FOREIGN KEY (device_id) REFERENCES devices(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE license_transfer_requests (
    id CHAR(36) NOT NULL, license_id CHAR(36) NOT NULL, old_device_id CHAR(36) NOT NULL,
    new_device_id CHAR(36) NOT NULL, status VARCHAR(32) NOT NULL, reason VARCHAR(500) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), resolved_at TIMESTAMP(6) NULL,
    PRIMARY KEY (id), CONSTRAINT fk_transfer_license FOREIGN KEY (license_id) REFERENCES licenses(id),
    CONSTRAINT fk_transfer_old_device FOREIGN KEY (old_device_id) REFERENCES devices(id),
    CONSTRAINT fk_transfer_new_device FOREIGN KEY (new_device_id) REFERENCES devices(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE account_recovery_cases (
    id CHAR(36) NOT NULL, user_id CHAR(36) NOT NULL, status VARCHAR(32) NOT NULL,
    reason VARCHAR(500) NOT NULL, evidence_ref VARCHAR(500) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), resolved_at TIMESTAMP(6) NULL,
    PRIMARY KEY (id), CONSTRAINT fk_recovery_user FOREIGN KEY (user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
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
