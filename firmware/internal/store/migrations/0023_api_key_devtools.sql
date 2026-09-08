-- 单把 API 密钥的开发工具策略。缺少配置行按三种订阅全关、额外模型为空处理。
-- 模型选择使用外键，模型被删除时同步清掉选择；密钥删除时两张子表级联清理。

CREATE TABLE api_key_devtool_configs (
    key_id                    INTEGER PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
    allow_codex_subscription  INTEGER NOT NULL DEFAULT 0 CHECK (allow_codex_subscription IN (0, 1)),
    allow_grok_subscription   INTEGER NOT NULL DEFAULT 0 CHECK (allow_grok_subscription IN (0, 1)),
    allow_claude_subscription INTEGER NOT NULL DEFAULT 0 CHECK (allow_claude_subscription IN (0, 1)),
    revision                  INTEGER NOT NULL CHECK (revision > 0),
    updated_at                TEXT    NOT NULL
);

CREATE TABLE api_key_devtool_models (
    key_id   INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    PRIMARY KEY (key_id, model_id)
);
