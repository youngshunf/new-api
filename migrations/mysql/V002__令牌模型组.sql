-- 本迁移不建业务表；第一条只确保框架账本存在（runner 约定）。
CREATE TABLE IF NOT EXISTS schema_migrations (
    migration_id varchar(128) PRIMARY KEY NOT NULL,
    checksum varchar(64) NOT NULL,
    created_time datetime(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- 令牌所属模型组（LLM 网关设计 §8.2.2）：空串表示不受模型组约束（上游用户自建令牌）。
ALTER TABLE tokens ADD COLUMN model_group varchar(32) NOT NULL DEFAULT '';
