-- 新表分类：voice_profiles=platform_mutable_entity；只存资源事实，不另建音频账务。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id TEXT PRIMARY KEY NOT NULL,
    checksum TEXT NOT NULL,
    created_time INTEGER NOT NULL
);
-- 幂等键只保存SHA256摘要，三方言无需依赖文本排序规则。
CREATE TABLE IF NOT EXISTS voice_profiles (
    voice_ref TEXT PRIMARY KEY NOT NULL,
    created_time INTEGER NOT NULL,
    updated_time INTEGER NOT NULL,
    revision INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    creator_token_id INTEGER NOT NULL,
    idempotency_key_digest TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    kind TEXT NOT NULL,
    language TEXT NOT NULL,
    target_model TEXT NOT NULL,
    channel_id INTEGER NOT NULL,
    provider_voice_id TEXT NULL,
    state TEXT NOT NULL,
    failure_reason TEXT NULL,
    retention_policy TEXT NOT NULL,
    delete_requested_time INTEGER NULL,
    delete_dispatched_time INTEGER NULL,
    deleted_time INTEGER NULL
);
CREATE UNIQUE INDEX idx_voice_creation_key ON voice_profiles(user_id, idempotency_key_digest);
