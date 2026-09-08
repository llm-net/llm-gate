-- 0026_drop_grok_agent: 清掉 Grok Build 订阅承载的 xAI Imagine 图片/视频行。
-- 设备不转发订阅侧出图/出视频：没有 /agents/v1/images|videos/* 路由，也没有
-- xai_image/xai_video 两个接口族与 grok_agent 上游类型；模型目录数据的 agents
-- 段只收文本模型。旧固件按目录数据建过的那组行——xai 两族的模型行、挂在
-- grok_agent 锚点上游上的来源行、锚点上游本身——在这里一次清掉；没连过
-- Grok 订阅的设备是空操作。
--
-- upstreams.type 的 CHECK 集合里保留 'grok_agent' 字面量：收紧它要再做一次
-- 表重建（0005/0009/0010/0016/0022 那条路径），而 Go 层已不认识该类型、不会
-- 再建这种行，不值得为一个空集合背一次重建。用量账本的历史行
-- （entry=imagine_image/imagine_video）是既成事实，不动。
--
-- 顺序按外键依赖：来源行 → 模型行 → 上游行（model_sources.upstream_id 是
-- NO ACTION，先删来源才删得掉上游；模型行本就级联，显式写出只为读起来清楚）。
DELETE FROM model_sources
 WHERE upstream_id IN (SELECT id FROM upstreams WHERE type = 'grok_agent')
    OR model_id IN (SELECT id FROM models WHERE family IN ('xai_image', 'xai_video'));
DELETE FROM models WHERE family IN ('xai_image', 'xai_video');
DELETE FROM upstreams WHERE type = 'grok_agent';
