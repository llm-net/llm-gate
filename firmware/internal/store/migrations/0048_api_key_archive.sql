-- 0048_api_key_archive: API密钥的「归档」态与不复用的主键。两件事：
-- ① api_keys 加 archived_at 列（NULL = 在用）。归档是删除的替代：凭据作废
--    （摘要换成不可命中的哨兵、封存明文清空、禁用位置 1），行本身与标签保留，
--    用量账本 / AIGC 任务 / 模型预览里的 key_id 仍能对回一个有名字的对象。
-- ② id 改为 AUTOINCREMENT：不带 AUTOINCREMENT 的 INTEGER PRIMARY KEY 在删掉
--    id 最大的那把密钥后会把同一个 id 发给下一把新签的密钥，新密钥就在账本里
--    继承了前任的历史。sqlite_sequence 同时按账本里出现过的最大 key_id 起步，
--    此前已被物理删除的密钥留下的历史行也不会被新密钥认领。
--
-- migrate:foreign_keys=off
-- ↑ 重建型迁移标记（store.applyRebuildMigration 识别此行）：SQLite 不能给既有表
--   改主键形态，只能新表建立→拷贝→DROP→改名，机理与改名方向的陷阱见 0005 的
--   头注释。api_key_devtool_configs / api_key_api_models 等子表的 REFERENCES
--   api_keys 字面原样保留，DROP→RENAME 之后自然指向新表，id 按原值拷贝。

CREATE TABLE api_keys_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    label          TEXT    NOT NULL DEFAULT '',
    key_digest     TEXT    NOT NULL UNIQUE,
    display_prefix TEXT    NOT NULL DEFAULT '',
    display_last4  TEXT    NOT NULL DEFAULT '',
    project_id     INTEGER,
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    last_used_at   TEXT,
    budget_day_micro   INTEGER,
    budget_week_micro  INTEGER,
    budget_month_micro INTEGER,
    rpm_limit          INTEGER,
    plaintext_sealed   TEXT NOT NULL DEFAULT '',
    metered_allowance_micro INTEGER NOT NULL DEFAULT 0
        CHECK (metered_allowance_micro >= 0),
    -- 归档时刻（UTC 毫秒文本，同库内其他时间戳）；NULL = 在用。归档不可逆。
    archived_at    TEXT
);
INSERT INTO api_keys_new (id, label, key_digest, display_prefix, display_last4, project_id,
                          disabled, created_at, last_used_at, budget_day_micro, budget_week_micro,
                          budget_month_micro, rpm_limit, plaintext_sealed, metered_allowance_micro)
     SELECT id, label, key_digest, display_prefix, display_last4, project_id,
            disabled, created_at, last_used_at, budget_day_micro, budget_week_micro,
            budget_month_micro, rpm_limit, plaintext_sealed, metered_allowance_micro
       FROM api_keys;
DROP TABLE api_keys;
ALTER TABLE api_keys_new RENAME TO api_keys;

-- 主键序列从「库里见过的最大 key_id」起步：在用密钥、账本、AIGC 任务与模型预览
-- 任务里出现过的 id 都算。RENAME 已把 sqlite_sequence 里的表名跟着改过来，这里
-- 整行重写。
DELETE FROM sqlite_sequence WHERE name = 'api_keys';
INSERT INTO sqlite_sequence (name, seq)
SELECT 'api_keys', MAX(m) FROM (
    SELECT COALESCE(MAX(id), 0) AS m FROM api_keys
    UNION ALL SELECT COALESCE(MAX(key_id), 0) FROM usage_hourly
    UNION ALL SELECT COALESCE(MAX(key_id), 0) FROM aigc_tasks
    UNION ALL SELECT COALESCE(MAX(key_id), 0) FROM preview_tasks
);
