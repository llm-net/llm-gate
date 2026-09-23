-- 0057_agent_host_model_service: 「智能体 → 主机/SoC」的类型词汇表加入 model_service
-- （模型服务节点：管理多台算力服务器、调度模型推理、对外提供任务 API、缓存模型文件、
-- 直接运行 Codex App Server / Claude Code，主机上跑守护进程 llmgate-modeld）。
--
-- migrate:foreign_keys=off
-- SQLite 不能改既有表的 CHECK，只能按 0005 的路径重建：新表临时名建立 → 数据按列拷贝
-- → DROP 旧表 → RENAME 顶替。agent_host_profiles / chats / runs / events 与 workspaces、
-- studio_workspaces 的 REFERENCES agent_hosts 字面原样保留，DROP→RENAME 之后自然指向新表，
-- 行里的 host_id 按原 id 拷贝，引用无损。agent_hosts 上没有显式索引或触发器。
--
-- devd_* 列从此记「这台主机上的守护进程」：工作节点是 llmgate-devd，模型服务节点是
-- llmgate-modeld；一台主机只有一种类型、只装一种守护进程，列名沿用不改。
CREATE TABLE agent_hosts_new (
    id              INTEGER PRIMARY KEY,
    name            TEXT NOT NULL DEFAULT '',
    address         TEXT NOT NULL,
    port            INTEGER NOT NULL,
    username        TEXT NOT NULL,
    host_key        TEXT NOT NULL DEFAULT '',
    key_fingerprint TEXT NOT NULL DEFAULT '',
    sudo_nopasswd   INTEGER NOT NULL DEFAULT 0,
    system          TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('ready', 'error')),
    last_error      TEXT NOT NULL DEFAULT '',
    last_checked_at TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    devd_version    TEXT NOT NULL DEFAULT '',
    devd_home       TEXT NOT NULL DEFAULT '',
    devd_tmux       INTEGER NOT NULL DEFAULT 0,
    devd_status     TEXT NOT NULL DEFAULT '' CHECK (devd_status IN ('', 'ready', 'error')),
    devd_last_error TEXT NOT NULL DEFAULT '',
    devd_checked_at TEXT,
    kind            TEXT NOT NULL DEFAULT 'worker' CHECK (kind IN ('managed', 'worker', 'model_service')),
    UNIQUE (username, address, port)
);
INSERT INTO agent_hosts_new (id, name, address, port, username, host_key, key_fingerprint, sudo_nopasswd, system,
    status, last_error, last_checked_at, created_at, updated_at,
    devd_version, devd_home, devd_tmux, devd_status, devd_last_error, devd_checked_at, kind)
  SELECT id, name, address, port, username, host_key, key_fingerprint, sudo_nopasswd, system,
    status, last_error, last_checked_at, created_at, updated_at,
    devd_version, devd_home, devd_tmux, devd_status, devd_last_error, devd_checked_at, kind
  FROM agent_hosts;
DROP TABLE agent_hosts;
ALTER TABLE agent_hosts_new RENAME TO agent_hosts;
