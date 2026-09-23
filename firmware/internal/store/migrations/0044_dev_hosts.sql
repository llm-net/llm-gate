-- 0044_dev_hosts: 「智能体 → 研发管理」纳管的开发主机清单。
-- 一行是一台装有 llmgate-devd 守护进程的主机：address + port（守护进程端口）唯一；
-- ssh_port / username 是远程安装时用的 SSH 连接事实（手动安装的行 username 为空）；
-- host_key 是首次经 SSH 连接时钉死的主机公钥（authorized_keys 形状）；identity 是
-- 首次连上守护进程时钉死的身份证书指纹，之后每次连接逐字比对；cert_fingerprint 是
-- 该主机上受信的设备研发证书指纹（与当前证书不一致即「证书待更新」）。
--
-- 表里没有任何凭据：安装用的主机口令只在那一次请求的内存里，设备证书私钥封存在
-- settings（AAD = 键名），两者都不入本表（§15.1）。
CREATE TABLE dev_hosts (
    id               INTEGER PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    address          TEXT NOT NULL,
    port             INTEGER NOT NULL,
    ssh_port         INTEGER NOT NULL DEFAULT 22,
    username         TEXT NOT NULL DEFAULT '',
    host_key         TEXT NOT NULL DEFAULT '',
    identity         TEXT NOT NULL DEFAULT '',
    cert_fingerprint TEXT NOT NULL DEFAULT '',
    system           TEXT NOT NULL DEFAULT '',
    daemon_version   TEXT NOT NULL DEFAULT '',
    home             TEXT NOT NULL DEFAULT '',
    tmux             INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL CHECK (status IN ('ready', 'error')),
    last_error       TEXT NOT NULL DEFAULT '',
    last_checked_at  TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    UNIQUE (address, port)
);
