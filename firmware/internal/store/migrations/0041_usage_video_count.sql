-- 账本补一列视频个数：Grok Imagine 的视频提交（imagine_video）是异步任务，
-- 设备只看到受理（request_id），看不到秒数，也不轮询清算——用量页因此只能
-- 显示「—」。图像门按张记（image_count），视频提交门就按受理个数记，
-- 与「张」同形、单位不同，所以另起一列而不借 image_count。
--
-- 与 0007 同形：普通 ALTER TABLE ADD COLUMN，不新建索引。存量行取缺省 0
-- （历史提交当时没记个数，不能事后编）。
ALTER TABLE usage_hourly ADD COLUMN video_count INTEGER NOT NULL DEFAULT 0;
