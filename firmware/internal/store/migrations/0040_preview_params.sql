-- 模型预览任务的生成参数快照（JSON）：比例、分辨率、质量、视频操作、时长、音频、音色，
-- 以及输入形态（是否带首帧 / 尾帧 / 源视频、参考图张数）。媒体本身不入行。
-- 空串表示没有额外参数（旧行与不带参数的提交）。
ALTER TABLE preview_tasks ADD COLUMN params TEXT NOT NULL DEFAULT '';
