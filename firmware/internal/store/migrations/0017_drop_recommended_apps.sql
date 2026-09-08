-- 0017_drop_recommended_apps: 清掉 settings 里的 recommended_apps 行。
-- 2026-08-15 白天的固件把「推荐应用」目录（recommended-apps.json）同步落库在这个键下，
-- 当晚产品改判为整页透传（docs/firmware-recommended-apps.md）——读它的代码已删，
-- 留着只是一段谁也不读的几十 KB JSON。settings 是 KV 表，没有 schema 可改；这条只是
-- 数据清理，装过那版固件的设备才真的有行可删，其余设备是空操作。
DELETE FROM settings WHERE key = 'recommended_apps';
