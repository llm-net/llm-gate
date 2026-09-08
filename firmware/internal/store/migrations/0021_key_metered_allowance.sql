-- 0021_key_metered_allowance: API密钥的按量额度剩余额。
--
-- 预算按自然日 / 周 / 月自动复位；按量额度是管理员给单把密钥追加的一次性
-- 兜底池，只有预算已经用尽后的新消费才扣减，本身不自动恢复。运行时扣减与
-- 管理员增减都在本列上做原子更新，避免并发消费覆盖人工调整。

ALTER TABLE api_keys
    ADD COLUMN metered_allowance_micro INTEGER NOT NULL DEFAULT 0
        CHECK (metered_allowance_micro >= 0);
