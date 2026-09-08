-- 0022_catalog_platforms：平台目录身份与稳定协议适配器分离。catalog_id 记录
-- 数据目录里的具体平台；type 继续只表达固件实现的协议适配器。base_url 是创建
-- 账号时确认并写入的端点快照，目录后续变化不会改写它。billing_mode 同样快照，
-- 让数据新增的订阅型兼容平台参与稳定的来源排序。
--
-- migrate:foreign_keys=off

CREATE TABLE upstreams_new (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE,
    type           TEXT    NOT NULL CHECK (type IN ('deepseek', 'ark', 'ark_plan', 'qwen_plan', 'minimax', 'openai_compat', 'anthropic_compat', 'mock', 'grok_agent')),
    catalog_id     TEXT    NOT NULL DEFAULT '',
    billing_mode   TEXT    NOT NULL DEFAULT '' CHECK (billing_mode IN ('', 'usage', 'subscription', 'none')),
    api_key_sealed TEXT    NOT NULL DEFAULT '',
    base_url       TEXT    NOT NULL DEFAULT '',
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);
INSERT INTO upstreams_new (id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, created_at, updated_at)
     SELECT id, name, type, type,
            CASE
              WHEN type IN ('ark_plan', 'qwen_plan', 'grok_agent') THEN 'subscription'
              WHEN type IN ('deepseek', 'ark', 'minimax') THEN 'usage'
              ELSE 'none'
            END,
            api_key_sealed, base_url, disabled, created_at, updated_at
       FROM upstreams;
DROP TABLE upstreams;
ALTER TABLE upstreams_new RENAME TO upstreams;
