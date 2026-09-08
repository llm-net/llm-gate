-- 0019_drop_users: 删除「用户」概念。设备只有一个管理员，登录退化成单一口令
-- （出厂 llm-gate，哈希存 settings 的 admin_password_hash 键），users 表连同
-- 用户级预算、备用额度、角色一并退场。
--
-- **本迁移是清空重建，不是数据迁移**：五张带用户维度的表全部 DROP 后按新形态
-- 重建，历史行一条不留。取舍是明摆着的——已下发的 API密钥会全部失效、历史账与
-- 审计清零，换来的是不必为一个即将消失的维度写一遍聚合搬运。上游账户、模型目录、
-- 模型来源、settings、Agent 订阅账号**原样保留**：那才是这台设备真正的配置。
--
-- DROP 顺序是必须的：api_keys 与 sessions 的 user_id 有 REFERENCES users(id)，
-- 外键开着时先 DROP users 会因子表仍在而失败（DROP 隐含 DELETE FROM 全表，立即
-- 触发外键校验）。先把两张子表删掉，users 就没有引用可挡，普通单事务路径即可
-- ——不需要 0005 那个独占一行的 migrate:foreign_keys=off 标记。

DROP TABLE sessions;
DROP TABLE api_keys;
DROP TABLE users;
DROP TABLE usage_hourly;
DROP TABLE aigc_tasks;
DROP TABLE audit_events;

-- ① api_keys：去掉 user_id 与它的索引。密钥级预算（0006/0008）、RPM 上限、
--    明文封存列（0012）全部保留——它们是密钥自己的属性，不随属主消失。
--    Key 明文的 SHA-256 十六进制摘要仍是库中唯一的可比对形态。
CREATE TABLE api_keys (
    id             INTEGER PRIMARY KEY,
    label          TEXT    NOT NULL DEFAULT '',
    key_digest     TEXT    NOT NULL UNIQUE,
    display_prefix TEXT    NOT NULL DEFAULT '',
    display_last4  TEXT    NOT NULL DEFAULT '',
    -- 预留列：账本/预算迭代挂项目归属（MVP §8 前向兼容），恒为 NULL。
    project_id     INTEGER,
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    last_used_at   TEXT,
    -- 密钥级预算（int64 微元，NULL = 不限）。窗口是设备本地时区的自然日 /
    -- 自然周（周一起算，ISO 口径）/ 自然月。准入检查序：RPM → 日 → 周 → 月。
    budget_day_micro   INTEGER,
    budget_week_micro  INTEGER,
    budget_month_micro INTEGER,
    -- 每分钟请求数上限（60s 滑窗，内存态计数，重启清零）；NULL = 不限。
    rpm_limit          INTEGER,
    -- 设备密钥 AES-256-GCM 密文（AAD = "apikey:<key_digest>"，密文钉死在本行
    -- 摘要上，挪到别的行解不开）；空串 = 明文未留存。
    plaintext_sealed   TEXT NOT NULL DEFAULT ''
);

-- ② sessions：去掉 user_id。会话不再指向谁，只表示「这个浏览器验过口令」。
--    时间戳仍是 UTC 固定宽度毫秒文本，字符串比较即时间比较
--    （DeleteExpiredSessions 依赖此性质）。
CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    -- 会话令牌的 SHA-256 十六进制摘要；令牌明文不入库（防库泄露重放）。
    token_digest TEXT    NOT NULL UNIQUE,
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    remote_ip    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

-- ③ audit_events：去掉 actor_user_id / actor_name。只有一个操作者，"谁做的"
--    这一栏恒等于同一个答案，留着只是每行一份噪声。追加式不变——Go 侧永远
--    只有 AppendAudit，没有任何更新/删除方法。
--    detail 不得含密码/令牌/Key 明文（§15.1 纪律延伸到审计字段）。
CREATE TABLE audit_events (
    id        INTEGER PRIMARY KEY,
    at        TEXT    NOT NULL,
    event     TEXT    NOT NULL,
    entity    TEXT    NOT NULL DEFAULT '',
    detail    TEXT    NOT NULL DEFAULT '',
    remote_ip TEXT    NOT NULL DEFAULT ''
);

-- ④ usage_hourly：去掉 user_id / user_name 两个维度列，唯一索引跟着收窄。
--    列注释的完整版见 0006——本文件只重复那些改动涉及的部分。
CREATE TABLE usage_hourly (
    -- 桶号 = unix 秒 / 3600，恒 UTC（与本库文本时间戳约定刻意不同，理由见 0006）。
    bucket_hour        INTEGER NOT NULL,
    -- 归属维度只剩密钥：id + 展示串快照。展示串（display_prefix…last4）建后
    -- 不可改，所以快照永不过期；密钥被删后本表的行照样留存——历史账不随密钥走
    -- （同 audit_events 与 aigc_tasks 的无外键先例，本表一律不设外键）。
    key_id             INTEGER NOT NULL,
    key_display        TEXT    NOT NULL DEFAULT '',
    -- 客户端可见模型名（上游原始名，架构 §11.1）。
    model_name         TEXT    NOT NULL DEFAULT '',
    -- 上游账户名快照；准入被拒（429）的行没走到选路，此列为空串。
    upstream_name      TEXT    NOT NULL DEFAULT '',
    entry              TEXT    NOT NULL DEFAULT '',
    kind               TEXT    NOT NULL DEFAULT '',
    requests           INTEGER NOT NULL DEFAULT 0,
    errors             INTEGER NOT NULL DEFAULT 0,
    rejected_requests  INTEGER NOT NULL DEFAULT 0,
    estimated_requests INTEGER NOT NULL DEFAULT 0,
    prompt_tokens      INTEGER NOT NULL DEFAULT 0,
    completion_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens       INTEGER NOT NULL DEFAULT 0,
    -- 非 token 形态的计费量（0007 加的两列）：秒是 MiniMax H3 的权威计价量
    -- （输出秒 + 输入秒同一单价，合成一列），张数是 H3 的输入参考图或
    -- Seedream 的出图。文本行两列恒为 0。
    video_seconds      INTEGER NOT NULL DEFAULT 0,
    image_count        INTEGER NOT NULL DEFAULT 0,
    -- 本桶消费额合计，int64 微元（1 元 = 10⁶ 微元）。
    cost_micro         INTEGER NOT NULL DEFAULT 0,
    duration_ms_sum    INTEGER NOT NULL DEFAULT 0
);
-- 维度唯一索引，也是本表唯一的索引：UPSERT 的冲突目标兼区间扫描的支撑。
CREATE UNIQUE INDEX idx_usage_hourly_dim
    ON usage_hourly (bucket_hour, key_id, model_name, upstream_name, entry);

-- ⑤ aigc_tasks：去掉 user_id / user_name，**归属裁决改按 key_id**。查询/取消
--    仍是「厂商任务 id + 归属同时命中才有行」，「不是你的」与「不存在」同回
--    ErrNotFound——不给存在性 oracle 这条性质原样保留，只是主语从用户换成密钥。
--    列注释的完整版见 0005；cost_micro / estimated 两列来自 0006 的清算加列。
CREATE TABLE aigc_tasks (
    id              TEXT    PRIMARY KEY,
    vendor_task_id  TEXT    NOT NULL,
    upstream_id     INTEGER NOT NULL,
    upstream_name   TEXT    NOT NULL,
    model_name      TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    key_id          INTEGER NOT NULL,
    key_display     TEXT    NOT NULL,
    status          TEXT    NOT NULL,
    error_code      TEXT    NOT NULL DEFAULT '',
    error_message   TEXT    NOT NULL DEFAULT '',
    usage_json      TEXT    NOT NULL DEFAULT '',
    has_video_input INTEGER NOT NULL DEFAULT 0,
    generate_audio  INTEGER NOT NULL DEFAULT 0,
    service_tier    TEXT    NOT NULL DEFAULT '',
    req_resolution  TEXT    NOT NULL DEFAULT '',
    req_duration    TEXT    NOT NULL DEFAULT '',
    content_url     TEXT    NOT NULL DEFAULT '',
    last_frame_url  TEXT    NOT NULL DEFAULT '',
    -- NULL = 未清算，这正是一次性清算的判据（SettleAIGCTask 的条件更新）。
    cost_micro      INTEGER,
    estimated       INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL
);
-- 归属查询与近期列表（key_id 等值 + created_at 排序/范围）。保留期清理的
-- created_at < ? 谓词有意不建单独索引（理由同 0005）。
CREATE INDEX idx_aigc_tasks_key_created ON aigc_tasks (key_id, created_at);
-- 同一上游的同一厂商任务只允许入库一次；跨上游撞出的同名 id 互不相干。
CREATE UNIQUE INDEX idx_aigc_tasks_vendor ON aigc_tasks (vendor_task_id, upstream_id);
