-- 本迁移不建业务表；第一条只确保框架账本存在（runner 约定）。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id TEXT PRIMARY KEY NOT NULL,
    checksum TEXT NOT NULL,
    created_time INTEGER NOT NULL
);
-- 上游令牌「允许查询用户额度」开关（7de10f2c6）。正式账本存在后 tokens 不再走 AutoMigrate，列由这里补。形状与上游 AutoMigrate 建出的一致（可空、无默认）：已有令牌读成 false，即保持关闭。
ALTER TABLE tokens ADD COLUMN allow_user_quota_query numeric NULL;
