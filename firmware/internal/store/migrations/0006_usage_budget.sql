-- 0006_usage_budget: 用量小时聚合表 + 定价/预算/清算加列（iteration-9 Phase 1）。
-- 时间戳惯例的一处**有意例外**见 usage_hourly.bucket_hour 的注释；其余各表
-- 照旧（UTC 固定宽度毫秒文本）。
--
-- 本文件**不是**重建型迁移：全部是 CREATE TABLE / CREATE INDEX / ALTER TABLE
-- ADD COLUMN，普通单事务路径即可，所以不带 0005 那个独占一行的
-- "migrate:foreign_keys=off" 标记（此处提及不会被 hasRebuildDirective 误判——
-- 它只认整行只有标记本身的形态）。

-- ① 用量小时聚合表：本迭代唯一的新表，也是**唯一的持久计量层**。
--    请求级明细刻意不落盘（SD 卡写放大的最大头），只进内存环形缓冲；
--    「谁/哪个模型/花了多少/什么趋势」由小时桶全部回答。
--    写入路径：请求路径零写库，internal/usage 的冲刷协程每 5 分钟一个事务
--    批量 UPSERT（AddUsageDeltas 的 `列 = 列 + excluded.列` 相加语义）。
CREATE TABLE usage_hourly (
    -- 桶号 = unix 秒 / 3600，恒 UTC。与本库的文本时间戳约定**刻意不同**：
    -- 桶号要做区间扫描与本地时区折算（预算窗口跟着设备本地钟表走），整数
    -- 最直接；本表没有逐条时间戳，所以不破坏「字符串比较即时间比较」的既有
    -- 约定面。
    --
    -- 已知精度边界：一小时的粒度只能精确表达**整小时偏移**时区的自然日/自然月
    -- 边界。目标部署 Asia/Shanghai(+8) 落在整点上、精确；而 +5:30 / +5:45 /
    -- +9:30 这类半小时偏移时区的本地零点落在桶中间，播种预算时无论把窗口起点
    -- 向下还是向上取整，都会带 ≤1 小时的偏差（向下会把昨天末尾的消费算进今天，
    -- 且它不会随时间滑出窗口）。取哪一侧由 internal/usage 裁决并在那里写明；
    -- 真要做到精确，得改桶粒度（一次前向迁移 + 一倍左右的行数），本迭代不做。
    bucket_hour        INTEGER NOT NULL,
    -- 归属维度：id + 快照名。用户名是登录标识、建后不可改，密钥展示串
    -- （display_prefix…last4）同样不可改，所以快照永不过期；用户/密钥被删后
    -- 本表的行照样留存——历史账不随人走（同 audit_events 与 aigc_tasks 的
    -- 无外键先例，本表对 users/api_keys/models/upstreams 一律不设外键）。
    user_id            INTEGER NOT NULL,
    user_name          TEXT    NOT NULL DEFAULT '',
    key_id             INTEGER NOT NULL,
    key_display        TEXT    NOT NULL DEFAULT '',
    -- 客户端可见模型名（上游原始名，架构 §11.1）。
    model_name         TEXT    NOT NULL DEFAULT '',
    -- 上游账户名快照；准入被拒（429）的行没走到选路，此列为空串。
    upstream_name      TEXT    NOT NULL DEFAULT '',
    -- 入口 chat|messages|video|image 与模型 kind text|video|image：管理员按
    -- 「文本/视频/图片」拆分消费的直接依据。刻意不设 CHECK——入口词汇的增补
    -- 不该要求一次重建型迁移（同 aigc_tasks.status 的处置）。
    entry              TEXT    NOT NULL DEFAULT '',
    kind               TEXT    NOT NULL DEFAULT '',
    requests           INTEGER NOT NULL DEFAULT 0,
    -- errors = 客户端可见的最终状态 ≥400。
    errors             INTEGER NOT NULL DEFAULT 0,
    -- 网关准入拒绝（429）单列：它们没到上游，混进 errors 会把「用户超限」
    -- 误读成「上游故障」。
    rejected_requests  INTEGER NOT NULL DEFAULT 0,
    -- 本桶内 token 含估算值的请求数（管理台图表脚注「含估算」的依据）。
    estimated_requests INTEGER NOT NULL DEFAULT 0,
    -- token 四分量按原始口径如实保留（两家 cache 口径相反，见 AGENTS.md），
    -- total_tokens 是按入口协议折算一次的统一辅读数。金额时代仍留 token 列：
    -- cache 命中率与「未定价模型用了多少」都靠它们回答。
    prompt_tokens      INTEGER NOT NULL DEFAULT 0,
    completion_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens       INTEGER NOT NULL DEFAULT 0,
    -- 本桶消费额合计，int64 微元（1 元 = 10⁶ 微元），按记账时点的目录价折算。
    -- 预算、图表、一切金额读数都用这一列。**不设 CHECK (>= 0)**，但落库的增量
    -- 事实上恒非负：金额一律出自 usage.Cost（末端 clampCost 钳在 [0, 上限]），
    -- 而长流的中间结算与冲正只动内存里的预算计数器、不进本表。不加约束是因为
    -- UPSERT 的 `列 = 列 + excluded.列` 会把违例报在难以归因的地方，不是因为
    -- 负值合法——库里真出现负数就是缺陷信号。
    cost_micro         INTEGER NOT NULL DEFAULT 0,
    -- 求均值用；峰值不存（治理场景用不上，省一列）。
    duration_ms_sum    INTEGER NOT NULL DEFAULT 0
);
-- 维度唯一索引，也是**本表唯一的索引**：UPSERT 的冲突目标兼区间扫描的支撑。
-- 量级（活跃维度组合 × 每 5 分钟一行、保留 usage_days 天）下全表扫描是毫秒级，
-- 写入纪律优先——实测不够再加索引是一次前向迁移的事。
-- kind 有意不在键内：它由 entry 唯一决定（chat/messages→text、video→video、
-- image→image），入键只会白占索引宽度。
CREATE UNIQUE INDEX idx_usage_hourly_dim
    ON usage_hourly (bucket_hour, user_id, key_id, model_name, upstream_name, entry);

-- ② 模型目录价：单列 JSON，**形态定字段**（文本三价 / Seedance 两档 /
--    H3 秒价档+附加 / 图片张价），唯一的读写方是 internal/usage 的计价函数与
--    管理台表单，SQLite 侧没有按价格查询的需求。离散三列方案已被否——形态间
--    字段集异构，并集稀疏且每加一种形态就要一次迁移。
--    值一律**整数微元**（管理台按 元/百万 token 录入并换算）；空串 = 未定价
--    （照常转发、金额记 0、管理台挂警示徽章），与显式 0 价（定价为免费，不
--    警示）是两个状态。存量模型升级后全部处于未定价态——升级后一切行为不变。
ALTER TABLE models ADD COLUMN pricing TEXT NOT NULL DEFAULT '';

-- ③ 预算列：NULL = 不限（升级后既有用户/密钥一律不受限，行为不变）。
--    单位微元，窗口是设备**本地时区**的自然日 / 自然月（管理员的心智模型是
--    「这个月的钱」；存储侧的桶号保持 UTC 无歧义，折算在 internal/usage）。
ALTER TABLE users ADD COLUMN budget_day_micro   INTEGER;
ALTER TABLE users ADD COLUMN budget_month_micro INTEGER;
ALTER TABLE api_keys ADD COLUMN budget_day_micro   INTEGER;
ALTER TABLE api_keys ADD COLUMN budget_month_micro INTEGER;
-- 每分钟请求数上限（60s 滑窗，内存态计数，重启清零）；NULL = 不限。
ALTER TABLE api_keys ADD COLUMN rpm_limit INTEGER;

-- ④ 异步任务的清算列（0005 建的 aigc_tasks）。cost_micro **NULL = 未清算**，
--    这正是一次性清算的判据：SettleAIGCTask 的条件更新只在 cost_micro IS NULL
--    时写入，于是「客户端查询路径顺手观测到完成」与「懒对账协程扫到它」重叠
--    时也只入账一次。estimated 标记该笔金额是否含估算成分（视频/图片不估算
--    token，此标位表示厂商 usage 缺失或已过查询窗口而按 0 元记）。
ALTER TABLE aigc_tasks ADD COLUMN cost_micro INTEGER;
ALTER TABLE aigc_tasks ADD COLUMN estimated  INTEGER NOT NULL DEFAULT 0;
