-- 模型预览结果的缩略图（短边 480 的 JPEG）保存在数据目录 preview/ 下，行里只记文件名
-- （<任务 ID>.thumb.jpg）；空串表示还没有缩略图（生成失败、结果不是可解码的图像，
-- 或视频的封面帧尚未由页面回传）。
ALTER TABLE preview_tasks ADD COLUMN thumb_file TEXT NOT NULL DEFAULT '';
