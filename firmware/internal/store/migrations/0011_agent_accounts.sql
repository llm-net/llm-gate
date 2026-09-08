-- 0011_agent_accounts: Agents（Codex 订阅代理）的账号与凭据（iteration-11 Phase 1）。
--
-- 与 upstreams 刻意分表（迭代 11 决策 2）：订阅账号的凭据是**可轮换的 OAuth
-- 句柄**（整份 auth.json：access/refresh/id token），与 upstreams 那张「一行一个
-- 静态 Key 上游」的模型形态根本不同。混进去既污染那张表的模型，也会把 codex
-- 这条不走模型目录的路强行塞进 (type, 协议) 端点矩阵里。
--
-- 纯 CREATE，**不带** `-- migrate:foreign_keys=off` 重建标记（照 0006/0007/0008
-- 先例；只有要改既有表 CHECK 的重建型迁移才需要那一行）。
CREATE TABLE agent_accounts (
    id               INTEGER PRIMARY KEY,
    -- provider 当前只有 'codex'，表形留给未来（claude 等）。**刻意不加 CHECK**：
    -- 这是「厂商清单」型的列，upstreams.type 的枚举 CHECK 已经为此付过三次
    -- 重建型迁移（0005/0009/0010）的代价——加一个新 provider 不该是一次重建。
    provider         TEXT NOT NULL,
    -- 管理员给这份订阅起的名字（展示用，可空）。
    label            TEXT NOT NULL DEFAULT '',
    -- OpenAI 侧账号标识（从 id_token 的 claim 提取）。**明文列，不是密钥物料**：
    -- 管理台靠它认「这是哪个账号」，代理靠它注入 ChatGPT-Account-ID。
    account_id       TEXT NOT NULL DEFAULT '',
    -- 启动器命令写进 ~/.codex/llmgate.config.toml 的模型名（可空 = 未设）。
    default_model    TEXT NOT NULL DEFAULT '',
    -- 整份 auth.json 的 AES-256-GCM 密文（随机 nonce 前置，整体 base64；设备密钥
    -- 见 <data_dir>/device-key，AAD 钉 'agent:<provider>'）。这是继上游 Key、
    -- 这是用设备密钥封存的订阅凭据。明文与密文都不进任何
    -- View / API 响应 / 日志 / 审计 detail（§15.1）。
    auth_json_sealed TEXT NOT NULL DEFAULT '',
    -- 三值状态：active 可用 / disabled 管理员停用 / auth_expired 刷新被确定性
    -- 拒绝（要管理员重新登录）。这里**加**了 CHECK：与 provider 不同，这是本包
    -- 自己拥有的封闭词汇表，写错一个值就是一台永远不 active、也说不出为什么的
    -- 代理，宁可在写入时就炸掉。真要加第四个值那是一次重建型迁移（0005 先例）。
    status           TEXT NOT NULL CHECK (status IN ('active', 'disabled', 'auth_expired')),
    -- 最近一次令牌刷新成功的时刻；NULL = 从未刷新过（刚连上就是这个状态）。
    last_refresh_at  TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    -- 单账户语义：同一 provider 只有一行，重复连接是**覆盖**而非增行。
    -- 唯一键刻意只有 provider——label 是 PATCH 可改字段，把它并进唯一键会让
    -- 「改个名再连一次」合法地插出第二行，单账户语义当场失效。
    UNIQUE (provider)
);
