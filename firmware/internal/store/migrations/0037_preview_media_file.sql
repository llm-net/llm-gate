-- 模型预览生成结果保存在设备数据目录 preview/ 下，行里只记文件名（<任务 ID>.<扩展名>）；
-- 空串表示没有本地文件（生成失败、尚未取回，或早于本列的旧任务只有平台 URL）。
ALTER TABLE preview_tasks ADD COLUMN media_file TEXT NOT NULL DEFAULT '';
