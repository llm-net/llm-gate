-- 0034_agent_multi_account: 开发工具订阅多账号。两件事：
-- ① agent_accounts 去掉 UNIQUE(provider)：同一种订阅可以录入多个账号，每行一份
--    独立封存的凭据；重新登录按行 id 覆盖，不再按 provider 覆盖。
-- ② api_key_devtool_configs 的四个布尔开关改成四个「钉死账号」列
--    <provider>_account_id：一把 Key 每种工具最多固定一个订阅账号，NULL = 未授权。
--    账号删除时置空（ON DELETE SET NULL），仓储删除路径同时递增受影响 Key 的
--    revision。既有开关为 1 的行回填当时该 provider 的唯一账号（旧 schema 下每种
--    订阅至多一行）；开关为 1 而没有账号可钉的行按未授权处理。
--
-- migrate:foreign_keys=off
-- ↑ 重建型迁移标记（store.applyRebuildMigration 识别此行）：SQLite 不能删除既有
--   表的 UNIQUE/CHECK，只能新表建立→拷贝→DROP→改名，机理与改名方向的陷阱见
--   0005 的头注释。两张表都按「新表用临时名建、DROP 旧表、RENAME 顶替」进行，
--   REFERENCES 里的名字从不被重命名。

-- ① agent_accounts 重建：与 0011 的唯一差异是去掉 UNIQUE (provider)，其余列语义
--    照抄 0011（密文列 auth_json_sealed 的 AAD 仍钉 'agent:<provider>'）。id 保持原值，
--    钉死账号列回填时靠它。
CREATE TABLE agent_accounts_new (
    id               INTEGER PRIMARY KEY,
    provider         TEXT NOT NULL,
    label            TEXT NOT NULL DEFAULT '',
    account_id       TEXT NOT NULL DEFAULT '',
    default_model    TEXT NOT NULL DEFAULT '',
    auth_json_sealed TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL CHECK (status IN ('active', 'disabled', 'auth_expired')),
    last_refresh_at  TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
INSERT INTO agent_accounts_new (id, provider, label, account_id, default_model,
                                auth_json_sealed, status, last_refresh_at, created_at, updated_at)
     SELECT id, provider, label, account_id, default_model,
            auth_json_sealed, status, last_refresh_at, created_at, updated_at
       FROM agent_accounts;
DROP TABLE agent_accounts;
ALTER TABLE agent_accounts_new RENAME TO agent_accounts;
CREATE INDEX agent_accounts_provider ON agent_accounts (provider, id);

-- ② api_key_devtool_configs 重建：四个开关列换成四个钉死账号列。key_id 与
--    revision / updated_at 语义照抄 0023；回填按旧开关 × 当时该 provider 的唯一行。
CREATE TABLE api_key_devtool_configs_new (
    key_id            INTEGER PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
    codex_account_id  INTEGER REFERENCES agent_accounts(id) ON DELETE SET NULL,
    grok_account_id   INTEGER REFERENCES agent_accounts(id) ON DELETE SET NULL,
    claude_account_id INTEGER REFERENCES agent_accounts(id) ON DELETE SET NULL,
    cursor_account_id INTEGER REFERENCES agent_accounts(id) ON DELETE SET NULL,
    revision          INTEGER NOT NULL CHECK (revision > 0),
    updated_at        TEXT    NOT NULL
);
INSERT INTO api_key_devtool_configs_new (key_id, codex_account_id, grok_account_id,
                                         claude_account_id, cursor_account_id, revision, updated_at)
     SELECT key_id,
            CASE WHEN allow_codex_subscription  = 1 THEN (SELECT MIN(id) FROM agent_accounts WHERE provider = 'codex')  END,
            CASE WHEN allow_grok_subscription   = 1 THEN (SELECT MIN(id) FROM agent_accounts WHERE provider = 'grok')   END,
            CASE WHEN allow_claude_subscription = 1 THEN (SELECT MIN(id) FROM agent_accounts WHERE provider = 'claude') END,
            CASE WHEN allow_cursor_subscription = 1 THEN (SELECT MIN(id) FROM agent_accounts WHERE provider = 'cursor') END,
            revision, updated_at
       FROM api_key_devtool_configs;
DROP TABLE api_key_devtool_configs;
ALTER TABLE api_key_devtool_configs_new RENAME TO api_key_devtool_configs;
