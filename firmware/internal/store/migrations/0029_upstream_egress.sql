-- 0029_upstream_egress: upstreams 增 egress_mode（inherit|direct|proxy）——该账号的出站
-- 方式覆盖：inherit 跟随设备级「模型与订阅接口」出口，direct / proxy 强制。既有行按
-- DEFAULT 'inherit'，行为与迁移前一致。设备级代理 profile 与各分类出口落 settings
--（egress.*），不在本表。

ALTER TABLE upstreams
    ADD COLUMN egress_mode TEXT NOT NULL DEFAULT 'inherit'
        CHECK (egress_mode IN ('inherit', 'direct', 'proxy'));
