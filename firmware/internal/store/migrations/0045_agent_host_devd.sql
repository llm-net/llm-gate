-- 0045_agent_host_devd: 「智能体 → 主机/SoC」每一行记下它有没有装开发台守护进程
-- llmgate-devd。守护进程沿用这一行已有的 SSH 连接事实（凭访问证书免密登录）安装，
-- 不再另起一张 dev_hosts 表：一台主机只登记一次。
--
-- devd_status 为空 = 没装（其余 devd_* 列全为零值）；ready / error 与 status 列同义，
-- 只是针对守护进程那一条连接。devd_identity 是首次连上守护进程时钉死的身份证书指纹，
-- 之后每次连接逐字比对；devd_cert_fingerprint 是主机上受信的设备研发证书指纹（与当前
-- 证书不一致即「证书待更新」）。表里仍没有任何凭据（§15.1）。
ALTER TABLE agent_hosts ADD COLUMN devd_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_hosts ADD COLUMN devd_identity TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_hosts ADD COLUMN devd_cert_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_hosts ADD COLUMN devd_version TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_hosts ADD COLUMN devd_home TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_hosts ADD COLUMN devd_tmux INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_hosts ADD COLUMN devd_status TEXT NOT NULL DEFAULT '' CHECK (devd_status IN ('', 'ready', 'error'));
ALTER TABLE agent_hosts ADD COLUMN devd_last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_hosts ADD COLUMN devd_checked_at TEXT;
DROP TABLE IF EXISTS dev_hosts;
