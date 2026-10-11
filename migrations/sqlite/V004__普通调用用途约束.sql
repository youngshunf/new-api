-- 本迁移不建业务表；第一条只确保框架账本存在（runner 约定）。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id TEXT PRIMARY KEY NOT NULL,
    checksum TEXT NOT NULL,
    created_time INTEGER NOT NULL
);
-- Runtime用空串持久表示缺席；普通call用途仅由可信租约入口写入。
ALTER TABLE tokens ADD COLUMN operation TEXT NOT NULL DEFAULT '';
