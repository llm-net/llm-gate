-- 0018_drop_legacy_cloud_settings: 删除已退役的设备上报开关与凭据。
--
-- 当前设备只匿名读取 LLM Gate官网静态文件，不再持有官网设备身份、上报开关
-- 或上报凭据。这三行来自旧 cloudlink；其中 cloud_secret 是设备密钥封存的敏感
-- 数据。代码不再读取它们后必须迁移清除，避免无主凭据继续留在存量数据库中。
-- settings 是通用 KV 表，其余本地配置（例如 external_url 与数据升级文件）不动。
DELETE FROM settings
WHERE key IN ('cloud_enabled', 'cloud_serial', 'cloud_secret');
