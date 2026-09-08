-- 单把 API 密钥经 API 调用可用的模型范围。缺少配置行 = 不限制（该 Key 能调
-- 目录里全部 API密钥接入 模型），既有密钥因此不受影响；restricted=1 才按
-- api_key_api_models 里的选择收窄。
--
-- 模型选择使用外键，模型被删除时同步清掉选择；密钥删除时两张子表级联清理。
-- 形状比照 0023 的开发工具两表。

CREATE TABLE api_key_api_model_configs (
    key_id     INTEGER PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
    restricted INTEGER NOT NULL DEFAULT 0 CHECK (restricted IN (0, 1)),
    revision   INTEGER NOT NULL CHECK (revision > 0),
    updated_at TEXT    NOT NULL
);

CREATE TABLE api_key_api_models (
    key_id   INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    PRIMARY KEY (key_id, model_id)
);
