-- 新表分类：schema_migrations=migration_ledger；audio_request_settlements=platform_mutable_entity。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id TEXT PRIMARY KEY NOT NULL,
    checksum TEXT NOT NULL,
    created_time INTEGER NOT NULL
);
ALTER TABLE tokens ADD COLUMN purpose TEXT NOT NULL DEFAULT 'generic';
ALTER TABLE tokens ADD COLUMN accounting_mode TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE tokens ADD COLUMN external_lease_id TEXT NULL;
CREATE UNIQUE INDEX idx_tokens_external_lease_id ON tokens(external_lease_id);
-- 查询唯一键只用于关联登记，不允许重放模型调用。
CREATE TABLE IF NOT EXISTS audio_request_settlements (
    audio_request_settlement_id TEXT PRIMARY KEY NOT NULL,
    created_time INTEGER NOT NULL,
    updated_time INTEGER NOT NULL,
    revision INTEGER NOT NULL,
    completed_time INTEGER NULL,
    external_lease_id TEXT NOT NULL,
    gateway_request_id TEXT NOT NULL,
    token_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    credential_generation INTEGER NOT NULL,
    relay_mode INTEGER NOT NULL,
    model_name TEXT NOT NULL,
    pre_consumed_quota INTEGER NOT NULL,
    funding_preference TEXT NOT NULL,
    subscription_applied_quota INTEGER NOT NULL,
    wallet_applied_quota INTEGER NOT NULL,
    token_applied_quota INTEGER NOT NULL,
    subscription_id INTEGER NULL,
    subscription_period_anchor INTEGER NULL,
    billing_status TEXT NOT NULL,
    terminal_action TEXT NULL,
    target_quota INTEGER NULL,
    dispatch_state TEXT NOT NULL,
    failure_code TEXT NULL
);
CREATE UNIQUE INDEX idx_audio_settlement_lookup ON audio_request_settlements(external_lease_id, gateway_request_id);
