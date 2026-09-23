-- 0049_agent_host_kind: 「智能体 → 主机/SoC」每一行带一个类型（kind），纳管时选定、之后不改：
--   managed  受控纳管：只能用 Agent远控，不能在主机上安装开发台守护进程 devd；
--   worker   工作节点：Agent远控之外还可安装 devd、打开开发台。
-- 已有行按 worker 补齐：它们可能已经装着 devd，缩成 managed 会让开发台无法使用。
ALTER TABLE agent_hosts ADD COLUMN kind TEXT NOT NULL DEFAULT 'worker' CHECK (kind IN ('managed', 'worker'));
