-- 0047_agent_host_chats: 「Agent远控」的多对话。
-- 一台主机可以开多个对话（agent_host_chats），每个对话新建时钉死 API 密钥、模型与推理
-- 档位（之后不改），并把当时的主机档案与最近操作渲染成开发者指令存进 instructions；
-- 指令与时间线事件各多一列 chat_id 标明归属（管理员改档案这类主机级事件留空）。
-- 对话随主机级联删除；删对话由代码删掉它的指令与对话内容事件、把操作记录留给主机。
-- 引擎设置不再是全局的：清掉旧的 codex_app_server.* 设置项。
CREATE TABLE agent_host_chats (
    id           TEXT PRIMARY KEY,
    host_id      INTEGER NOT NULL REFERENCES agent_hosts(id) ON DELETE CASCADE,
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
CREATE INDEX agent_host_chats_host ON agent_host_chats (host_id, created_at);

ALTER TABLE agent_host_runs ADD COLUMN chat_id TEXT NOT NULL DEFAULT '';
CREATE INDEX agent_host_runs_chat ON agent_host_runs (chat_id, created_at);

ALTER TABLE agent_host_events ADD COLUMN chat_id TEXT NOT NULL DEFAULT '';
CREATE INDEX agent_host_events_chat ON agent_host_events (chat_id, id);

DELETE FROM settings WHERE key IN ('codex_app_server.key_id', 'codex_app_server.model', 'codex_app_server.effort');
