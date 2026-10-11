-- 本迁移不建业务表；第一条只确保框架账本存在（runner 约定）。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id varchar(128) PRIMARY KEY NOT NULL,
    checksum varchar(64) NOT NULL,
    created_time timestamptz NOT NULL
);
-- Runtime用空串持久表示缺席；普通call用途仅由可信租约入口写入。
ALTER TABLE tokens ADD COLUMN operation varchar(32) NOT NULL DEFAULT '';
