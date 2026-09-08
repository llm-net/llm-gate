-- 0002_upstreams_models: 上游账户与模型目录三表（iteration-5 Phase 1）。
-- SQLite 是上游/模型/来源的唯一事实源（决策 1）；YAML 的 upstreams 与
-- logical_models 降级为启动时的幂等导入。时间戳惯例同 0001（UTC 固定宽度毫秒文本）。

-- 上游账户。type 枚举与 internal/config 的 Upstream* 常量同步维护。
CREATE TABLE upstreams (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE,
    type           TEXT    NOT NULL CHECK (type IN ('deepseek', 'ark', 'ark_plan', 'mock')),
    -- 上游凭证的 AES-256-GCM 密文（随机 nonce 前置，整体 base64；设备密钥见
    -- <data_dir>/device-key，架构 §12）。明文不入库、不落日志、不进审计 detail
    -- 也不进管理 API 响应（§15.1）；无凭证的上游（mock）此列为空串。
    api_key_sealed TEXT    NOT NULL DEFAULT '',
    -- 仅 dev/mock 覆盖内置端点表用；产品上游留空走内置 (type, 协议) 端点表。
    base_url       TEXT    NOT NULL DEFAULT '',
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);

-- 模型目录。name 就是客户端可见的原始模型名（iteration-5 反转架构 §11.1 的
-- 对客隐藏决策：隐藏对象改为上游账户与来源侧差异，模型名本身不再改写）。
CREATE TABLE models (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
);

-- 模型来源：一个模型可挂多个上游，按 priority 选路（小者优先，同值按 id 即创建序）。
-- 删模型级联删其来源；被来源引用的上游禁止删除，由管理 API 转 409。
-- upstream_id 刻意用默认的 NO ACTION 而不是 ON DELETE RESTRICT：两者对单行
-- DELETE 行为一致（父行仍被引用即失败），但 SQLite 用触发器程序实现 RESTRICT，
-- 报的是 SQLITE_CONSTRAINT_TRIGGER 而非 SQLITE_CONSTRAINT_FOREIGNKEY，
-- store.mapErr 只认后者。改这一行前先改 mapErr。
CREATE TABLE model_sources (
    id                INTEGER PRIMARY KEY,
    model_id          INTEGER NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    upstream_id       INTEGER NOT NULL REFERENCES upstreams (id),
    -- 来源侧的模型 ID；空串表示"与模型名相同"（路由时取模型名）。
    upstream_model_id TEXT    NOT NULL DEFAULT '',
    priority          INTEGER NOT NULL DEFAULT 100,
    -- 预留列：本迭代无任何读写语义（决策 7），口径（请求数/token/金额）与
    -- 生效逻辑随账本/计量迭代拍板；届时口径不合可前向迁移加列。
    quota             INTEGER,
    disabled          INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL,
    -- 同一模型在同一上游上只允许一条来源。
    UNIQUE (model_id, upstream_id)
);

CREATE INDEX idx_model_sources_model_id    ON model_sources (model_id);
CREATE INDEX idx_model_sources_upstream_id ON model_sources (upstream_id);
