-- 0056_drop_split_catalog_settings: 清掉 settings 里合并前那两份目录数据文件的行。
-- 数据升级改为只取一份模型目录文件（键 model_catalog，internal/admin/catalogsync.go），
-- 官方价目（official_pricing）与平台模型信息（platform_models）两个键的读取代码已删，
-- 留着只是两段谁也不读的百余 KB JSON。settings 是 KV 表，没有 schema 可改；这条只是
-- 数据清理，同步过旧文件的设备才真的有行可删，其余设备是空操作。
DELETE FROM settings WHERE key IN ('official_pricing', 'platform_models');
