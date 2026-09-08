-- 0032_drop_openai_video: OpenAI Videos 不再是设备的协议面。
--
-- 声明为 openai_video 的模型行整行删除（来源经 model_sources 的外键级联一起走），
-- 经泛型 openai_compat 账号受理的视频任务行同样删除——设备上再没有任何路由能
-- 查询或取消它们，留着只会让懒对账每轮报「上游类型无任务适配器」。历史用量
-- （usage_hourly）按模型名记账，不动。
DELETE FROM aigc_tasks
 WHERE kind = 'video'
   AND upstream_id IN (SELECT id FROM upstreams WHERE type = 'openai_compat');
DELETE FROM models WHERE family = 'openai_video';
