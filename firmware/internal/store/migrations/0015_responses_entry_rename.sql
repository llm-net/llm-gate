-- 0015_responses_entry_rename: usage_hourly.entry 的两个 Responses 取值改名
-- （2026-08-13 产品裁决：`responses` 这个本名归标准 Responses 目录面
-- POST /v1/responses（初版曾记作 responses_catalog）；Agents 订阅代理
-- POST /agents/v1/responses 改记 `responses_agents`。词汇表在
-- internal/usage/usage.go，契约见 docs/firmware-usage-metering.md）。
--
-- 历史行一并改写：entry 只改词汇不改历史的话，旧的订阅代理流量会顶着目录面
-- 的名字进报表——0 元的订阅行和按目录价记账的目录面行混在同一个 entry 下，
-- 正是这次改名要消除的混淆。
--
-- 顺序有讲究：先把 responses 腾出来，再让 responses_catalog 搬进去。倒过来
-- 第一步就会把两类流量合并进同一取值（还可能撞 (bucket_hour, user_id,
-- key_id, model_name, upstream_name, entry) 唯一索引），且无从回退。
UPDATE usage_hourly SET entry = 'responses_agents' WHERE entry = 'responses';
UPDATE usage_hourly SET entry = 'responses' WHERE entry = 'responses_catalog';
