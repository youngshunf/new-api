-- 新表分类：voice_profiles=platform_mutable_entity；只存资源事实，不另建音频账务。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id varchar(128) PRIMARY KEY NOT NULL,
    checksum varvarchar(64) NOT NULL,
    created_time timestamptz NOT NULL
);
-- 幂等键只保存SHA256摘要，三方言无需依赖文本排序规则。
CREATE TABLE IF NOT EXISTS voice_profiles (
    voice_ref uuid PRIMARY KEY NOT NULL,
    created_time timestamptz NOT NULL,
    updated_time timestamptz NOT NULL,
    revision bigint NOT NULL,
    user_id bigint NOT NULL,
    creator_token_id bigint NOT NULL,
    idempotency_key_digest varchar(64) NOT NULL,
    request_digest varchar(64) NOT NULL,
    kind varchar(16) NOT NULL,
    language varchar(16) NOT NULL,
    target_model varchar(256) NOT NULL,
    channel_id bigint NOT NULL,
    provider_voice_id varchar(256) NULL,
    state varchar(16) NOT NULL,
    failure_reason varchar(512) NULL,
    retention_policy varchar(32) NOT NULL,
    delete_requested_time timestamptz NULL,
    delete_dispatched_time timestamptz NULL,
    deleted_time timestamptz NULL
);
CREATE UNIQUE INDEX idx_voice_creation_key ON voice_profiles(user_id, idempotency_key_digest);
