-- +goose Up
CREATE TABLE admin_actions (
    id CHAR(36) NOT NULL, actor_user_id CHAR(36) NULL, action VARCHAR(128) NOT NULL,
    correlation_id VARCHAR(255) NOT NULL, reason VARCHAR(1000) NULL, target_type VARCHAR(128) NULL,
    target_id VARCHAR(255) NULL, before_state JSON NULL, after_state JSON NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY (id),
    KEY ix_admin_actions_actor (actor_user_id), KEY ix_admin_actions_correlation (correlation_id),
    CONSTRAINT fk_admin_actions_actor FOREIGN KEY (actor_user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE audit_logs (
    id CHAR(36) NOT NULL, user_id CHAR(36) NULL, actor_user_id CHAR(36) NULL,
    action VARCHAR(128) NOT NULL, correlation_id VARCHAR(255) NOT NULL,
    reason VARCHAR(1000) NULL, target_type VARCHAR(128) NULL, target_id VARCHAR(255) NULL,
    metadata JSON NULL, created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY (id),
    KEY ix_audit_logs_user_created (user_id, created_at), KEY ix_audit_logs_correlation (correlation_id),
    CONSTRAINT fk_audit_logs_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_audit_logs_actor FOREIGN KEY (actor_user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE usage_records (
    id CHAR(36) NOT NULL, user_id CHAR(36) NOT NULL, device_id CHAR(36) NULL, studio_session_id CHAR(36) NULL,
    operation VARCHAR(128) NOT NULL, outcome VARCHAR(64) NOT NULL, units BIGINT UNSIGNED NOT NULL DEFAULT 1,
    request_id VARCHAR(255) NULL, metadata JSON NULL, occurred_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id), KEY ix_usage_user_occurred (user_id, occurred_at), KEY ix_usage_device (device_id),
    UNIQUE KEY uq_usage_id_user (id, user_id),
    CONSTRAINT fk_usage_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_usage_device_owner FOREIGN KEY (device_id, user_id) REFERENCES devices(id, user_id),
    CONSTRAINT fk_usage_studio_owner FOREIGN KEY (studio_session_id, user_id) REFERENCES studio_sessions(id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose StatementBegin
CREATE TRIGGER audit_logs_no_update BEFORE UPDATE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs is append-only';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER audit_logs_no_delete BEFORE DELETE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs is append-only';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER usage_records_no_update BEFORE UPDATE ON usage_records FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'usage_records is append-only';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER usage_records_no_delete BEFORE DELETE ON usage_records FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'usage_records is append-only';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER admin_actions_no_update BEFORE UPDATE ON admin_actions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'admin_actions is append-only';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER admin_actions_no_delete BEFORE DELETE ON admin_actions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'admin_actions is append-only';
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS admin_actions_no_delete;
DROP TRIGGER IF EXISTS admin_actions_no_update;
DROP TRIGGER IF EXISTS usage_records_no_delete;
DROP TRIGGER IF EXISTS usage_records_no_update;
DROP TRIGGER IF EXISTS audit_logs_no_delete;
DROP TRIGGER IF EXISTS audit_logs_no_update;
DROP TABLE IF EXISTS usage_records;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS admin_actions;
