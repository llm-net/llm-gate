-- 0005_aigc: 视频/图片转发面的存储基座（iteration-8 Phase 1）。三件事：
-- ① upstreams.type 的 CHECK 扩入 'minimax'（重建路径）；② models 加 kind 列；
-- ③ 新建 aigc_tasks 异步任务表。时间戳惯例同 0001（UTC 固定宽度毫秒文本）。
--
-- migrate:foreign_keys=off
-- ↑ 重建型迁移标记（本仓库首个，store.applyRebuildMigration 识别此行）：
--   本文件在一条独占连接上、**事务之外**先 PRAGMA foreign_keys=OFF 再于单
--   事务内执行，提交前跑 PRAGMA foreign_key_check 全库自证重建没留悬空引用，
--   提交/回滚后恢复 foreign_keys=ON（恢复失败即废弃该连接，绝不带着 OFF 回池）。
--
-- 为什么必须关外键：SQLite 不能修改既有表的 CHECK 约束，唯一路径是
-- 「新表以临时名建立 → 数据拷贝 → DROP 旧表 → RENAME 顶替」
-- （sqlite.org/lang_altertable.html「Making Other Kinds Of Table Schema
-- Changes」）；外键开着时 DROP TABLE upstreams 会因 model_sources 仍有行引用
-- 它而直接失败（DROP 隐含 DELETE FROM 全表，立即触发外键校验）。而 PRAGMA
-- foreign_keys 在事务内是 no-op，普通迁移路径（整文件跑在单事务里）根本关
-- 不掉它，所以需要专门的执行路径。
--
-- 改名方向的陷阱（给后人）：务必像下面这样「新表用临时名建、DROP 旧表、把
-- 新表 RENAME 成正名」，绝不要反过来把旧表 RENAME 成 *_old 留档——外键开启
-- 时 RENAME 会把其他表 REFERENCES 子句里的旧名一并改写（model_sources 会
-- 跟着指向 upstreams_old），schema 就被悄悄改坏。本方向从不重命名被引用的
-- 名字：model_sources 的 REFERENCES upstreams 字面原样保留，DROP→RENAME
-- 之后自然指向新表，行里的 upstream_id 值又按原 id 拷贝，引用因此无损。

-- ① upstreams 重建：与 0002 的唯一差异是 type 的 CHECK 集合加入 'minimax'。
--    注意 DB 的 CHECK 在此**先行**放开：internal/config 的 Upstream* 常量、
--    admin 上游端点与内置端点表随迭代 8 Phase 2 跟进——在那之前 YAML 与
--    管理 API 仍拒绝 minimax，库里不会出现该类型的行（0002「与 config 同步
--    维护」的约定在 Phase 2 收口后恢复成立）。其余列语义照抄 0002：
--    api_key_sealed 是上游凭证的 AES-256-GCM 密文（明文不入库不落日志不进
--    审计与管理响应，§15.1），base_url 仅 dev/mock 覆盖内置端点表用。
--    数据按列名拷贝、id 保持原值。
CREATE TABLE upstreams_new (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE,
    type           TEXT    NOT NULL CHECK (type IN ('deepseek', 'ark', 'ark_plan', 'minimax', 'mock')),
    api_key_sealed TEXT    NOT NULL DEFAULT '',
    base_url       TEXT    NOT NULL DEFAULT '',
    disabled       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);
INSERT INTO upstreams_new (id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at)
     SELECT id, name, type, api_key_sealed, base_url, disabled, created_at, updated_at FROM upstreams;
DROP TABLE upstreams;
ALTER TABLE upstreams_new RENAME TO upstreams;
-- 索引重建：upstreams 上没有显式 CREATE INDEX——唯一索引来自 name 列的内联
-- UNIQUE，随新表定义自动重建，无需额外语句。将来若重建带显式索引/触发器的
-- 表，DROP 会连它们一起删，必须在 RENAME 之后逐条原文重建。

-- ② 模型种类：text（文本对话，既有全部模型的回填值）| video（异步视频任务）
--    | image（同步出图）。建后不可改（改 kind 等于换模型，照上游 type 先例，
--    store 层不提供写方法）；数据面入口按 kind 选路，互不越界。
ALTER TABLE models ADD COLUMN kind TEXT NOT NULL DEFAULT 'text' CHECK (kind IN ('text', 'video', 'image'));

-- ③ 异步任务表（首个用途：视频生成）。行由设备在厂商创建成功后写入，同时
--    是 iteration-9 计费的账单事实来源——因此对 upstreams/users/api_keys 一律
--    **不设外键**并冗余快照名称列：上游/用户/Key 之后被删，任务行也必须原样
--    留存（同 audit_events 无外键的先例）；归属校验只按 user_id 值比对。
CREATE TABLE aigc_tasks (
    -- 设备签发的任务 id（agt- 前缀不透明串），客户端唯一可见的任务标识，
    -- 在调上游之前生成（迭代 8 方案要点 2）。
    id              TEXT    PRIMARY KEY,
    -- 厂商侧任务 id：只用于设备→厂商的查询/取消/下载，绝不进客户端响应。
    vendor_task_id  TEXT    NOT NULL,
    -- 任务创建后来源锁定：查询/取消/下载一律走这里钉死的上游，不再故障切换。
    upstream_id     INTEGER NOT NULL,
    upstream_name   TEXT    NOT NULL,
    model_name      TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    user_id         INTEGER NOT NULL,
    user_name       TEXT    NOT NULL,
    key_id          INTEGER NOT NULL,
    key_display     TEXT    NOT NULL,
    -- 设备契约的归一状态词汇（queued/running/succeeded/failed/cancelled/
    -- expired…），由 gateway 适配器归一后写入；刻意不设 CHECK——状态词汇的
    -- 增补不该要求一次重建型迁移。
    status          TEXT    NOT NULL,
    error_code      TEXT    NOT NULL DEFAULT '',
    -- 错误信息截断存储（写入方裁到有限长度再入库）。
    error_message   TEXT    NOT NULL DEFAULT '',
    -- 厂商 usage 原文 JSON（两家形态异构，iteration-9 计价按 kind+上游类型
    -- 解读；计价函数只读这里，不再碰线上的字节）。
    usage_json      TEXT    NOT NULL DEFAULT '',
    -- 计费特征列：提交时即固化（Seedance 分档按是否含视频输入定档，此刻
    -- 不存以后就得猜）。
    has_video_input INTEGER NOT NULL DEFAULT 0,
    generate_audio  INTEGER NOT NULL DEFAULT 0,
    service_tier    TEXT    NOT NULL DEFAULT '',
    req_resolution  TEXT    NOT NULL DEFAULT '',
    req_duration    TEXT    NOT NULL DEFAULT '',
    -- 厂商产物 URL 的时效副本，仅供排障：会过期；客户端拿到的恒是设备自己
    -- 合成的下载地址，厂商 URL 不出设备。
    content_url     TEXT    NOT NULL DEFAULT '',
    last_frame_url  TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL
);
-- 归属查询与近期列表（user_id 等值 + created_at 排序/范围）。保留期清理的
-- created_at < ? 谓词**有意不建**单独索引：行数被 14 天保留期钉住，全表
-- 扫描可忽略，省一个索引就省每次插入的一份写放大（SD 卡纪律）。
CREATE INDEX idx_aigc_tasks_user_created ON aigc_tasks (user_id, created_at);
-- 同一上游的同一厂商任务只允许入库一次；跨上游撞出的同名 id 互不相干。
CREATE UNIQUE INDEX idx_aigc_tasks_vendor ON aigc_tasks (vendor_task_id, upstream_id);
