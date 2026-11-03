-- 新表分类：channel_request_budget_usages=platform_mutable_entity；一渠道一期一行调用计数，旧期行不清理。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id TEXT PRIMARY KEY NOT NULL,
    checksum TEXT NOT NULL,
    created_time INTEGER NOT NULL
);
-- period_key 为北京时间分期键（月 2026-11、日 2026-11-03）；used_count 只经单条 upsert 加一。
-- 复合主键列表不留空格：SQLite 驱动的 DDL 解析只认逗号紧跟列名，否则漏判主键列。
CREATE TABLE IF NOT EXISTS channel_request_budget_usages (
    channel_id INTEGER NOT NULL,
    period_key TEXT NOT NULL,
    used_count INTEGER NOT NULL,
    updated_time INTEGER NOT NULL,
    PRIMARY KEY (channel_id,period_key)
);
