-- +goose Up
CREATE TABLE oauth_session_consents (
    web_session_id CHAR(36) NOT NULL,
    client_id CHAR(36) NOT NULL,
    grant_id CHAR(36) NOT NULL,
    requested_scopes JSON NOT NULL,
    resource VARCHAR(2048) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (web_session_id, client_id),
    KEY ix_oauth_session_consents_client (client_id),
    KEY ix_oauth_session_consents_grant (grant_id),
    CONSTRAINT fk_oauth_session_consents_session FOREIGN KEY (web_session_id) REFERENCES web_sessions(id) ON DELETE CASCADE,
    CONSTRAINT fk_oauth_session_consents_client FOREIGN KEY (client_id) REFERENCES oauth_clients(id) ON DELETE CASCADE,
    CONSTRAINT fk_oauth_session_consents_grant FOREIGN KEY (grant_id) REFERENCES oauth_grants(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE IF EXISTS oauth_session_consents;
