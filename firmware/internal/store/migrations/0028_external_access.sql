-- 0028_external_access: 公网接入方式收敛到 external_access.mode（none|manual|cloudflare）。
-- 外网映射与 Cloudflare Tunnel 互斥但分别保留设置：手动登记的地址迁到
-- external_access.manual_url，已登记的地址即刻成为 manual 模式；没登记过的设备
-- 保持 none（不写任何行，读取端把缺省当 none）。旧键 external_url 不再读取，删掉。
INSERT OR REPLACE INTO settings (key, value, updated_at)
SELECT 'external_access.manual_url', value, updated_at
FROM settings
WHERE key = 'external_url' AND value <> '';

INSERT OR REPLACE INTO settings (key, value, updated_at)
SELECT 'external_access.mode', 'manual', updated_at
FROM settings
WHERE key = 'external_url' AND value <> '';

DELETE FROM settings WHERE key = 'external_url';
