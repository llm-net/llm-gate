-- 0043_agent_host_agent: 「智能体 → 主机/SoC」的主机智能体数据。
-- 三张表都挂在 agent_hosts 上（解除纳管随行级联删除）：
--   agent_host_profiles  每台主机一份 Markdown 主机档案（智能体与管理员都能改）；
--   agent_host_runs      提交给智能体的指令（一条一次执行，按提交顺序逐个跑）；
--   agent_host_events    时间线兼操作日志：指令、回复、在主机上执行的每条命令、
--                        写过的文件、档案变更与会话起止，按 id 单调可分页、可审计。
-- 指令里的图片只在内存里交给引擎，不入库（行只记张数）；表里没有凭据。
CREATE TABLE agent_host_profiles (
    host_id    INTEGER PRIMARY KEY REFERENCES agent_hosts(id) ON DELETE CASCADE,
    content    TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

CREATE TABLE agent_host_runs (
    id            TEXT PRIMARY KEY,
    host_id       INTEGER NOT NULL REFERENCES agent_hosts(id) ON DELETE CASCADE,
    status        TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    text          TEXT NOT NULL,
    image_count   INTEGER NOT NULL DEFAULT 0,
    engine        TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    started_at    TEXT,
    finished_at   TEXT
);
CREATE INDEX agent_host_runs_host ON agent_host_runs (host_id, created_at);

CREATE TABLE agent_host_events (
    id          INTEGER PRIMARY KEY,
    host_id     INTEGER NOT NULL REFERENCES agent_hosts(id) ON DELETE CASCADE,
    run_id      TEXT NOT NULL DEFAULT '',
    at          TEXT NOT NULL,
    kind        TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    meta        TEXT NOT NULL DEFAULT '',
    exit_code   INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX agent_host_events_host ON agent_host_events (host_id, id);
