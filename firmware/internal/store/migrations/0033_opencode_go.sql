-- 0033_opencode_go: upstreams.type 的 CHECK 扩入 'opencode_go'——OpenCode Go
-- 包月套餐（opencode.ai/zen/go）：内置端点表里的文本双入口类型，Chat 与 Messages
-- 同一端点根；与 ark_plan / qwen_plan 同为「预付额度先用满」的订阅型
-- （store.billingRankSQL 的存量空 billing_mode 分支同步扩入）。重建路径照
-- 0005/0009/0010/0016/0022 先例（SQLite 不能修改既有表的 CHECK，只能新表建立→
-- 拷贝→DROP→改名），完整机理说明见 0005 的头注释，此处不重复。
--
-- migrate:foreign_keys=off
-- ↑ 重建型迁移标记（store.applyRebuildMigration 识别此行）：外键关闭的独占
--   连接 + 单事务 + 提交前 PRAGMA foreign_key_check 全库自证。

-- upstreams 重建：列集合是 0022 的表加 0029 追加在末尾的 egress_mode，与当前表
-- 逐列相同；与现表的唯一差异是 type 的 CHECK 集合加入 'opencode_go'。数据按列名
-- 拷贝、id 保持原值，model_sources / aigc_tasks 的 REFERENCES upstreams 字面原样
-- 保留（DROP→RENAME 后自然指向新表）。
CREATE TABLE upstreams_new (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE,
    type           TEXT    NOT NULL CHECK (type IN ('deepseek', 'ark', 'ark_plan', 'qwen_plan', 'opencode_go', 'minimax', 'openai_compat', 'anthropic_compat', 'mock', 'grok_agent')),
    catalog_id     TEXT    NOT NULL DEFAULT '',
    billing_mode   TEXT    NOT NULL DEFAULT '' CHECK (billing_mode IN ('', 'usage', 'subscription', 'none')),
    api_key_sealed TEXT    NOT NULL DEFAULT '',
    base_url       TEXT    NOT NULL DEFAULT '',
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    egress_mode    TEXT    NOT NULL DEFAULT 'inherit'
        CHECK (egress_mode IN ('inherit', 'direct', 'proxy'))
);
INSERT INTO upstreams_new (id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, created_at, updated_at, egress_mode)
     SELECT id, name, type, catalog_id, billing_mode, api_key_sealed, base_url, disabled, created_at, updated_at, egress_mode FROM upstreams;
DROP TABLE upstreams;
ALTER TABLE upstreams_new RENAME TO upstreams;
-- upstreams 上没有显式 CREATE INDEX（唯一索引来自 name 的内联 UNIQUE，随新表
-- 定义自动重建），无需额外语句——同 0005/0009/0010/0022 的说明。
