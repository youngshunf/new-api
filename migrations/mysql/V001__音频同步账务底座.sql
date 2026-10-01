-- 新表分类：schema_migrations=migration_ledger；audio_request_settlements=platform_mutable_entity。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id varchar(128) PRIMARY KEY NOT NULL,
    checksum varchar(64) NOT NULL,
    created_time datetime(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
ALTER TABLE tokens ADD COLUMN purpose varchar(32) NOT NULL DEFAULT 'generic';
ALTER TABLE tokens ADD COLUMN accounting_mode varchar(16) NOT NULL DEFAULT 'legacy';
ALTER TABLE tokens ADD COLUMN external_lease_id varchar(64) NULL;
CREATE UNIQUE INDEX idx_tokens_external_lease_id ON tokens(external_lease_id);
-- 查询唯一键只用于关联登记，不允许重放模型调用；时间固定UTC毫秒精度。
CREATE TABLE IF NOT EXISTS audio_request_settlements (
    audio_request_settlement_id char(36) PRIMARY KEY NOT NULL,
    created_time datetime(3) NOT NULL,
    updated_time datetime(3) NOT NULL,
    revision bigint NOT NULL,
    completed_time datetime(3) NULL,
    external_lease_id varchar(64) NOT NULL,
    gateway_request_id varchar(128) NOT NULL,
    token_id bigint NOT NULL,
    user_id bigint NOT NULL,
    credential_generation bigint NOT NULL,
    relay_mode bigint NOT NULL,
    model_name varchar(256) NOT NULL,
    pre_consumed_quota bigint NOT NULL,
    funding_preference varchar(32) NOT NULL,
    subscription_applied_quota bigint NOT NULL,
    wallet_applied_quota bigint NOT NULL,
    token_applied_quota bigint NOT NULL,
    subscription_id bigint NULL,
    subscription_period_anchor bigint NULL,
    billing_status varchar(16) NOT NULL,
    terminal_action varchar(16) NULL,
    target_quota bigint NULL,
    dispatch_state varchar(16) NOT NULL,
    failure_code varchar(64) NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE UNIQUE INDEX idx_audio_settlement_lookup ON audio_request_settlements(external_lease_id, gateway_request_id);
