-- System One 上游与语义判断模型；保留所有 ID、来源与授权外键。
-- migrate:foreign_keys=off

CREATE TABLE upstreams_new (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE,
    type           TEXT    NOT NULL CHECK (type IN ('deepseek', 'ark', 'ark_plan', 'qwen_plan', 'opencode_go', 'minimax', 'openai_compat', 'anthropic_compat', 'systemone', 'mock', 'grok_agent')),
    catalog_id     TEXT    NOT NULL DEFAULT '',
    billing_mode   TEXT    NOT NULL DEFAULT '' CHECK (billing_mode IN ('', 'usage', 'subscription', 'none')),
    api_key_sealed TEXT    NOT NULL DEFAULT '',
    base_url       TEXT    NOT NULL DEFAULT '',
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

CREATE TABLE models_new (
 id INTEGER PRIMARY KEY,
 name TEXT NOT NULL UNIQUE,
 disabled INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 kind TEXT NOT NULL DEFAULT 'text' CHECK (kind IN ('text', 'video', 'image', 'systemone')),
 pricing TEXT NOT NULL DEFAULT '',
 entry_openai INTEGER NOT NULL DEFAULT 1,
 entry_anthropic INTEGER NOT NULL DEFAULT 1,
 family TEXT NOT NULL DEFAULT '',
 entry_responses INTEGER NOT NULL DEFAULT 1
);
INSERT INTO models_new (id, name, disabled, created_at, updated_at, kind, pricing, entry_openai, entry_anthropic, family, entry_responses)
 SELECT id, name, disabled, created_at, updated_at, kind, pricing, entry_openai, entry_anthropic, family, entry_responses FROM models;
DROP TABLE models;
ALTER TABLE models_new RENAME TO models;
