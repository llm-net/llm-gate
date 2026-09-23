-- 0042_agent_hosts: 「智能体 → 主机/SoC」纳管的主机清单。
-- 一行是一台可被智能体远程管理的主机或 SoC 开发板：连接三元组（用户名、地址、
-- 端口）唯一，host_key 是首次纳管时钉死的主机公钥（authorized_keys 形状，之后
-- 每次连接逐字比对），key_fingerprint 是该主机上已安装的设备访问证书公钥指纹
-- （与当前证书不一致即「证书待更新」）。
--
-- 表里没有任何凭据：纳管用的主机口令只在那一次请求的内存里，公钥本就是公开值，
-- 设备私钥封存在 settings（AAD = 键名），两者都不入本表（§15.1）。
CREATE TABLE agent_hosts (
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
    UNIQUE (username, address, port)
);
