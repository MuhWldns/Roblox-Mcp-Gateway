-- +goose Up
CREATE TABLE oauth_clients (
    id CHAR(36) NOT NULL,
    client_id VARCHAR(255) NOT NULL,
    client_name VARCHAR(255) NOT NULL,
    redirect_uris JSON NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_oauth_clients_client_id (client_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE oauth_authorization_codes (
    id CHAR(36) NOT NULL,
    code_digest BINARY(32) NOT NULL,
    user_id CHAR(36) NOT NULL,
    client_id CHAR(36) NOT NULL,
    redirect_uri VARCHAR(2048) NOT NULL,
    code_challenge VARCHAR(128) NOT NULL,
    scopes JSON NOT NULL,
    device_id CHAR(36) NULL,
    studio_session_id CHAR(36) NULL,
    resource VARCHAR(2048) NOT NULL,
    expires_at TIMESTAMP(6) NOT NULL,
    consumed_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), UNIQUE KEY uq_oauth_codes_digest (code_digest),
    CONSTRAINT fk_oauth_codes_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_oauth_codes_client FOREIGN KEY (client_id) REFERENCES oauth_clients(id),
    CONSTRAINT fk_oauth_codes_device FOREIGN KEY (device_id) REFERENCES devices(id),
    CONSTRAINT fk_oauth_codes_studio FOREIGN KEY (studio_session_id) REFERENCES studio_sessions(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE oauth_grants (
    id CHAR(36) NOT NULL, user_id CHAR(36) NOT NULL, client_id CHAR(36) NOT NULL,
    device_id CHAR(36) NULL, studio_session_id CHAR(36) NULL, scopes JSON NOT NULL,
    resource VARCHAR(2048) NOT NULL, created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    revoked_at TIMESTAMP(6) NULL, PRIMARY KEY (id),
    UNIQUE KEY uq_oauth_grant_user_client_device (user_id, client_id, device_id),
    CONSTRAINT fk_oauth_grants_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_oauth_grants_client FOREIGN KEY (client_id) REFERENCES oauth_clients(id),
    CONSTRAINT fk_oauth_grants_device FOREIGN KEY (device_id) REFERENCES devices(id),
    CONSTRAINT fk_oauth_grants_studio FOREIGN KEY (studio_session_id) REFERENCES studio_sessions(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE oauth_access_tokens (
    id CHAR(36) NOT NULL, grant_id CHAR(36) NOT NULL, token_digest BINARY(32) NOT NULL,
    expires_at TIMESTAMP(6) NOT NULL, revoked_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY (id),
    UNIQUE KEY uq_oauth_access_digest (token_digest),
    CONSTRAINT fk_oauth_access_grant FOREIGN KEY (grant_id) REFERENCES oauth_grants(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE oauth_refresh_tokens (
    id CHAR(36) NOT NULL, grant_id CHAR(36) NOT NULL, family_id CHAR(36) NOT NULL,
    parent_id CHAR(36) NULL, token_digest BINARY(32) NOT NULL, expires_at TIMESTAMP(6) NOT NULL,
    used_at TIMESTAMP(6) NULL, revoked_at TIMESTAMP(6) NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY (id),
    UNIQUE KEY uq_oauth_refresh_digest (token_digest), KEY ix_oauth_refresh_family (family_id),
    CONSTRAINT fk_oauth_refresh_grant FOREIGN KEY (grant_id) REFERENCES oauth_grants(id),
    CONSTRAINT fk_oauth_refresh_parent FOREIGN KEY (parent_id) REFERENCES oauth_refresh_tokens(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE IF EXISTS oauth_refresh_tokens;
DROP TABLE IF EXISTS oauth_access_tokens;
DROP TABLE IF EXISTS oauth_grants;
DROP TABLE IF EXISTS oauth_authorization_codes;
DROP TABLE IF EXISTS oauth_clients;
