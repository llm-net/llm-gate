-- 媒体生成任务：设备自己发起的图像 / 视频生成（internal/mediagen）。表由 preview_tasks 改名，
-- 原有列与行原样保留；新增来源、归属、批次、后端、操作与输入形态。
--   origin    page（管理员或 Key 持有人在页面提交）/ studio（创作工作空间）/ cli（gate media）
--   owner     来源自己的归属信息（JSON）；studio：workspace_id / chat_id / run_id / name
--   batch_id  一次提交出多个候选时同批共用的 ULID；单个提交为空
--   backend   承载这次生成的后端名；订阅后端与 provider 同值
--   operation generate / edit / extend；空 = 读取时从 params 里的旧键补出
--   inputs    输入形态快照（JSON，各角色带了几个）；空 = 读取时从 params 里的旧键补出
ALTER TABLE preview_tasks RENAME TO media_jobs;
DROP INDEX preview_tasks_created;
DROP INDEX preview_tasks_key_created;
ALTER TABLE media_jobs ADD COLUMN origin TEXT NOT NULL DEFAULT 'page';
ALTER TABLE media_jobs ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE media_jobs ADD COLUMN batch_id TEXT NOT NULL DEFAULT '';
ALTER TABLE media_jobs ADD COLUMN backend TEXT NOT NULL DEFAULT '';
ALTER TABLE media_jobs ADD COLUMN operation TEXT NOT NULL DEFAULT '';
ALTER TABLE media_jobs ADD COLUMN inputs TEXT NOT NULL DEFAULT '';
UPDATE media_jobs SET backend = provider;
CREATE INDEX media_jobs_origin_created ON media_jobs(origin, created_at DESC);
CREATE INDEX media_jobs_key_created ON media_jobs(key_id, created_at DESC);
