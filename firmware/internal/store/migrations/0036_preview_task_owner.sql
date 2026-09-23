-- 模型预览任务的归属：管理员发起的任务 key_id 为 0；凭 API 密钥自证发起的任务
-- 记发起 Key 的行 id 与展示串快照（"前缀…末4位"，密钥删除后仍能辨认是谁发起的）。
ALTER TABLE preview_tasks ADD COLUMN key_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE preview_tasks ADD COLUMN key_display TEXT NOT NULL DEFAULT '';
CREATE INDEX preview_tasks_key_created ON preview_tasks(key_id, created_at DESC);
