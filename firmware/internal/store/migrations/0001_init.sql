-- 0001_init: 账号体系基础四表（iteration-4 Phase 1）。
-- 时间戳一律为 UTC 固定宽度毫秒文本（2006-01-02T15:04:05.000Z），
-- 字符串比较即时间比较（DeleteExpiredSessions 依赖此性质）。

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    name          TEXT    NOT NULL UNIQUE,
    role          TEXT    NOT NULL CHECK (role IN ('admin', 'member')),
    -- Argon2id PHC 串；member 无登录能力，恒为 NULL（架构 §12 人类登录与业务 Key 分离）。
    password_hash TEXT,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);

CREATE TABLE api_keys (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users (id),
    label          TEXT    NOT NULL DEFAULT '',
    -- Key 明文的 SHA-256 十六进制摘要；明文不入库（架构 §12 Key 摘要存储）。
    key_digest     TEXT    NOT NULL UNIQUE,
    display_prefix TEXT    NOT NULL DEFAULT '',
    display_last4  TEXT    NOT NULL DEFAULT '',
    -- 预留列：账本/预算迭代挂项目归属（MVP §8 前向兼容），本迭代恒为 NULL。
    project_id     INTEGER,
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    last_used_at   TEXT
);

CREATE INDEX idx_api_keys_user_id ON api_keys (user_id);

CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    -- 会话令牌的 SHA-256 十六进制摘要；令牌明文不入库（防库泄露重放）。
    token_digest TEXT    NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES users (id),
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    remote_ip    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_sessions_user_id    ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

-- 追加式审计（架构 §12）：Go 侧只有 AppendAudit，无任何更新/删除方法。
-- detail 不得含密码/初始化码/令牌/Key 明文（§15.1 纪律延伸到审计字段）。
CREATE TABLE audit_events (
    id            INTEGER PRIMARY KEY,
    at            TEXT    NOT NULL,
    actor_user_id INTEGER,
    actor_name    TEXT    NOT NULL DEFAULT '',
    event         TEXT    NOT NULL,
    entity        TEXT    NOT NULL DEFAULT '',
    detail        TEXT    NOT NULL DEFAULT '',
    remote_ip     TEXT    NOT NULL DEFAULT ''
);
