-- +goose Up
ALTER TABLE user_identities MODIFY COLUMN provider_subject VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL;
ALTER TABLE trial_entitlement_identities MODIFY COLUMN provider_subject VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL;

-- +goose Down
ALTER TABLE user_identities MODIFY COLUMN provider_subject VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL;
ALTER TABLE trial_entitlement_identities MODIFY COLUMN provider_subject VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL;
