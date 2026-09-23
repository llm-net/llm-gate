-- 0052_studio_workspaces: 工作空间分两种类型（kind）。
--   dev     开发工作空间：一台工作节点上的目录（原有形态，host_id 必填）；
--   studio  创作工作空间：设备自己数据目录下的目录（<data_dir>/workspaces/<id>），没有主机，
--           由 Codex App Server 驱动的智能体在里面生成 / 整理图像与视频素材。
-- SQLite 不能把 host_id 从 NOT NULL 改成可空，整表重建后再建索引：同一台主机上空间名唯一，
-- 设备上的创作工作空间之间空间名也唯一（两条部分唯一索引）。
CREATE TABLE workspaces_v2 (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL DEFAULT 'dev' CHECK (kind IN ('dev', 'studio')),
    host_id       INTEGER REFERENCES agent_hosts(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    path          TEXT NOT NULL,
    repo_url      TEXT NOT NULL DEFAULT '',
    branch        TEXT NOT NULL DEFAULT '',
    credential_id TEXT REFERENCES credentials(id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
INSERT INTO workspaces_v2 (id, kind, host_id, name, path, repo_url, branch, credential_id, created_at, updated_at)
    SELECT id, 'dev', host_id, name, path, repo_url, branch, credential_id, created_at, updated_at FROM workspaces;
DROP TABLE workspaces;
ALTER TABLE workspaces_v2 RENAME TO workspaces;
CREATE INDEX workspaces_host ON workspaces (host_id, id);
CREATE UNIQUE INDEX workspaces_host_name ON workspaces (host_id, name) WHERE host_id IS NOT NULL;
CREATE UNIQUE INDEX workspaces_studio_name ON workspaces (name) WHERE host_id IS NULL;

-- 创作工作空间的智能体数据，形态同 Agent远控（0043 / 0047），全部挂在 workspaces 上：
--   studio_chats   对话：新建时钉死 API 密钥、模型、推理档位与开发者指令；
--   studio_runs    提交给智能体的指令（按提交顺序逐条跑）；
--   studio_events  时间线：指令、回复、推理摘要、工具调用、生成记录、文件变更、会话起止；
--   studio_files   目录里每个文件的元数据（种类、大小、尺寸、来源、生成它的提示词 / 模型 /
--                  参数）；目录本身是真值，行只是附注，文件没了行也跟着清。
CREATE TABLE studio_chats (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    title        TEXT NOT NULL DEFAULT '',
    engine       TEXT NOT NULL DEFAULT '',
    key_id       INTEGER NOT NULL DEFAULT 0,
    key_display  TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    effort       TEXT NOT NULL DEFAULT '',
    instructions TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX studio_chats_workspace ON studio_chats (workspace_id, updated_at);

CREATE TABLE studio_runs (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    chat_id       TEXT NOT NULL REFERENCES studio_chats(id) ON DELETE CASCADE,
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
CREATE INDEX studio_runs_chat ON studio_runs (chat_id, created_at);

CREATE TABLE studio_events (
    id           INTEGER PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    chat_id      TEXT NOT NULL REFERENCES studio_chats(id) ON DELETE CASCADE,
    run_id       TEXT NOT NULL DEFAULT '',
    at           TEXT NOT NULL,
    kind         TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',
    meta         TEXT NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX studio_events_chat ON studio_events (chat_id, id);

CREATE TABLE studio_files (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL DEFAULT 'other',
    mime         TEXT NOT NULL DEFAULT '',
    bytes        INTEGER NOT NULL DEFAULT 0,
    width        INTEGER NOT NULL DEFAULT 0,
    height       INTEGER NOT NULL DEFAULT 0,
    origin       TEXT NOT NULL DEFAULT 'upload',
    provider     TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    prompt       TEXT NOT NULL DEFAULT '',
    params       TEXT NOT NULL DEFAULT '',
    chat_id      TEXT NOT NULL DEFAULT '',
    run_id       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (workspace_id, name)
);
