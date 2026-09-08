-- 0008_week_reserve: 周预算列 + 用户备用额度列（2026-08-09 直改，未走迭代流程）。
-- 全部是 ALTER TABLE ADD COLUMN，普通单事务路径即可——不同于 0005，本迁移
-- 不需要重建表，也就不带那个独占一行的外键标记。

-- ① 周预算：NULL = 不限（升级后存量用户/密钥一律不受限，行为不变）。
--    单位微元，窗口是设备**本地时区**的自然周（周一 00:00 起算，ISO 口径）。
--    准入检查序插在日与月之间：密钥 RPM → 密钥日 → 密钥周 → 密钥月 →
--    用户日 → 用户周 → 用户月。
ALTER TABLE users    ADD COLUMN budget_week_micro INTEGER;
ALTER TABLE api_keys ADD COLUMN budget_week_micro INTEGER;

-- ② 备用额度（仅用户级）：**剩余额**，非负整数微元，0 = 没有。与预算列的
--    NULL 语义刻意不同——备用额度是只减不增的存量池，「不限」无从谈起，
--    所以 NOT NULL DEFAULT 0。用户级日/周/月预算任一用尽后，新消费从这里
--    整笔扣减，直至扣光（密钥级预算不受它兜底）。
--    这一列是余量的**唯一权威**：usage_hourly 只保留 usage_days 天，算不出
--    「有史以来扣了多少」，所以运行时扣减（internal/usage 每个冲刷周期批量
--    UPDATE）与管理员增减（AdjustUserReserve）都以原子更新落在本列上，
--    绝不从账本重建。
ALTER TABLE users    ADD COLUMN budget_reserve_micro INTEGER NOT NULL DEFAULT 0;
