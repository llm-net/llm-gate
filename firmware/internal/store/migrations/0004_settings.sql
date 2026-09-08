-- 0004_settings: 设备级单值配置的通用键值表。
-- 首个用途是「外网映射地址」——管理员在自己的网络边界上（反向代理、端口
-- 映射、DDNS 等）为这台设备配好的、从设备外可达的基址；设备只记录展示，
-- 使用助手页据此告诉用户「从设备外怎么连」。单值配置走键值表而非各开一列，
-- 后续零散设置项无需再加迁移。
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
