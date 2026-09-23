-- 0046_agent_host_devd_over_ssh: 开发台守护进程不再有自己的 TLS 端口、身份证书与
-- 受信证书集合——设备经这台主机的免密 SSH 连接直达它的本机 Unix socket，鉴权与对端
-- 身份全由那条 SSH 连接（访问证书 + 钉死的主机公钥）承担。于是行里只剩守护进程的
-- 自述与最近一次检查结果；设备侧那张「研发证书」连同封存的私钥一并清掉。
ALTER TABLE agent_hosts DROP COLUMN devd_port;
ALTER TABLE agent_hosts DROP COLUMN devd_identity;
ALTER TABLE agent_hosts DROP COLUMN devd_cert_fingerprint;
DELETE FROM settings WHERE key LIKE 'dev_host.%';
