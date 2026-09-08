-- 0009_openai_compat: upstreams.type 的 CHECK 扩入 'openai_compat'——通用
-- OpenAI 兼容适配类型（2026-08-09）：无内置端点，地址由 base_url 显式给出，
-- 只服务 openai_chat 单协议，作为特化平台之外的兜底接入。重建路径照 0005
-- 先例（SQLite 不能修改既有表的 CHECK，只能新表建立→拷贝→DROP→改名），
-- 完整机理说明见 0005 的头注释，此处不重复。
--
-- migrate:foreign_keys=off
-- ↑ 重建型迁移标记（store.applyRebuildMigration 识别此行）：外键关闭的独占
--   连接 + 单事务 + 提交前 PRAGMA foreign_key_check 全库自证。

-- upstreams 重建：与 0005 的唯一差异是 type 的 CHECK 集合加入 'openai_compat'。
-- 其余列语义照抄 0005/0002；数据按列名拷贝、id 保持原值，model_sources 的
-- REFERENCES upstreams 字面原样保留（DROP→RENAME 后自然指向新表）。
CREATE TABLE upstreams_new (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE,
    type           TEXT    NOT NULL CHECK (type IN ('deepseek', 'ark', 'ark_plan', 'minimax', 'openai_compat', 'mock')),
    api_key_sealed TEXT    NOT NULL DEFAULT '',
    base_url       TEXT    NOT NULL DEFAULT '',
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);
INSERT INTO upstreams_new (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at)
     SELECT id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at FROM upstreams;
DROP TABLE upstreams;
ALTER TABLE upstreams_new RENAME TO upstreams;
-- upstreams 上没有显式 CREATE INDEX（唯一索引来自 name 的内联 UNIQUE，随新表
-- 定义自动重建），无需额外语句——同 0005 的说明。
