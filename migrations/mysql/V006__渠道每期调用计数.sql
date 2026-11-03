-- 新表分类：channel_request_budget_usages=platform_mutable_entity；一渠道一期一行调用计数，旧期行不清理。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id varchar(128) PRIMARY KEY NOT NULL,
    checksum varchar(64) NOT NULL,
    created_time datetime(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- period_key 为北京时间分期键（月 2026-11、日 2026-11-03）；used_count 只经单条 upsert 加一。
CREATE TABLE IF NOT EXISTS channel_request_budget_usages (
    channel_id bigint NOT NULL,
    period_key varchar(16) NOT NULL,
    used_count bigint NOT NULL,
    updated_time datetime(3) NOT NULL,
    PRIMARY KEY (channel_id,period_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
