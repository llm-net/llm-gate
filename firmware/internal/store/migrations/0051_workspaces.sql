-- 0051_workspaces: 「智能体 → 工作空间」。
-- 一行是一台工作节点上的一个工作空间：host_id 是它所在的主机（随主机解除纳管级联删除），
-- name 是空间名（也是主机上 ~/workspaces/<name> 的目录名，同一台主机上唯一），path 是
-- 创建时在主机上落成的绝对路径；repo_url / branch 是创建时克隆的仓库与分支（可空），
-- credential_id 是克隆时用的 git 凭证（凭证删掉即置空，空间本身还在）。
CREATE TABLE workspaces (
    id            TEXT PRIMARY KEY,
    host_id       INTEGER NOT NULL REFERENCES agent_hosts(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    path          TEXT NOT NULL,
    repo_url      TEXT NOT NULL DEFAULT '',
    branch        TEXT NOT NULL DEFAULT '',
    credential_id TEXT REFERENCES credentials(id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (host_id, name)
);
CREATE INDEX workspaces_host ON workspaces (host_id, id);
