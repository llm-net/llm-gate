-- 模型预览任务的完成时间：任务首次写成终态（succeeded / failed / expired）的时刻，
-- 之后补文件、补缩略图等写回不再改动它；空串表示尚未到终态。
-- 已到终态的旧行以最后更新时间回填。
ALTER TABLE preview_tasks ADD COLUMN finished_at TEXT NOT NULL DEFAULT '';
UPDATE preview_tasks SET finished_at=updated_at WHERE status IN ('succeeded', 'failed', 'expired') AND finished_at='';
