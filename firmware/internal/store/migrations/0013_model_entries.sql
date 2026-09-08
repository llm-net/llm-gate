-- 0013_model_entries: models 表加双调用入口开关（2026-08-12 产品决定：文本
-- 模型可按模型勾选开放哪些调用入口）。普通 ALTER TABLE ADD COLUMN，不带重建
-- 标记。
--
-- 1 = 开。默认双开，既有行行为不变；仅 kind=text 有语义——选路的 kind 闸门
-- 先于入口开关，视频/图片入口不读这两列。管理层保证 text 模型至少开一个
-- （400 entries_required）；库层不设 CHECK——两列全 0 的行为等同「双入口都
-- 404」，与停用一致且可恢复，不值得为它背一次 CHECK 重建（见 0005 的教训）。
ALTER TABLE models ADD COLUMN entry_openai INTEGER NOT NULL DEFAULT 1;
ALTER TABLE models ADD COLUMN entry_anthropic INTEGER NOT NULL DEFAULT 1;
