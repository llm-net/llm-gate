-- 通用适配按协议面保存地址；保留既有账号与来源。
-- migrate:foreign_keys=off

CREATE TABLE upstreams_new (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE,
    type           TEXT    NOT NULL CHECK (type IN ('deepseek', 'ark', 'ark_plan', 'qwen_plan', 'opencode_go', 'minimax', 'openai_compat', 'anthropic_compat', 'systemone', 'generic', 'mock', 'grok_agent')),
    catalog_id     TEXT    NOT NULL DEFAULT '',
    billing_mode   TEXT    NOT NULL DEFAULT '' CHECK (billing_mode IN ('', 'usage', 'subscription', 'none')),
    api_key_sealed TEXT    NOT NULL DEFAULT '',
    base_url       TEXT    NOT NULL DEFAULT '',
    protocol_urls  TEXT    NOT NULL DEFAULT '',
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    egress_mode    TEXT    NOT NULL DEFAULT 'inherit'
        CHECK (egress_mode IN ('inherit', 'direct', 'proxy'))
);
INSERT INTO upstreams_new (id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, created_at, updated_at, egress_mode)
     SELECT id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, created_at, updated_at, egress_mode FROM upstreams;
DROP TABLE upstreams;
ALTER TABLE upstreams_new RENAME TO upstreams;
