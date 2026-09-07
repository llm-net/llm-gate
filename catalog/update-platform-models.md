# 任务：更新平台模型信息

目标：让 `platform-models.json` 与各平台当前公示的端点与型号一致，三类接入（API 按量计费、API 订阅套餐、工具订阅）都覆盖。先读 [AGENTS.md](AGENTS.md)。

## 范围

1. **复核已有平台**：本次范围内的平台按 `source` 打开官方页面，核对固定端点根 `base_url`、`billing_mode` 与 `models` 里每个型号的 ID；型号上新补进选单，官方下架的删掉；页面地址变了就改 `source`。用户只指定一个平台或型号时不顺带全量更新。
2. **补齐三类**：
   - **API 按量计费**（`billing_mode: usage`）：厂商原生按量 API 与常用聚合平台。新平台必须有固定 HTTPS 端点根、Bearer Key（OpenAI 兼容）或 `x-api-key`（Anthropic 兼容）鉴权，以及 OpenAI Chat Completions 或 Anthropic Messages 兼容接口。
   - **API 订阅套餐**（`billing_mode: subscription`）：各厂商的 Coding Plan、Token Plan、会员 API。端点或 Key 与按量不通用时**必须独立成条**；套餐同时提供 OpenAI 与 Anthropic 兼容入口时按适配器各建一条（参照 `kimi_code_plan` 与 `kimi_code_plan_anthropic`）。待核对的候选：MiniMax Coding Plan、阶跃星辰 Step Plan、腾讯云 Token Plan、GLM Coding Plan 的 Anthropic 兼容入口。
   - **工具订阅**（`agents` 段）：Codex、Grok Build、Claude Code、Cursor 订阅的文本计价型号，以各厂商官方模型页为准；前三者每个型号都要在 `official-pricing.json` 里有带同名、同 `agent` 标记的价目条目。Cursor 在本文件的 `models[].pricing` 内维护官方标准模型价，设备只读展示并按精确 ID 计费，不改变 CLI 的模型发现。
3. **不收**：需要签名、短期令牌、项目 / 地域 / 部署名的平台（AWS Bedrock、Vertex AI、Azure OpenAI 等）；语音、嵌入、重排序、实时音频型号；只能经私有部署访问的地址。图片与视频型号只收固件已有协议面的平台（火山方舟、MiniMax）。

## 接入核对顺序

1. **确认平台身份**：API 文档、模型列表、账单与 Key 属于哪家平台；模型厂商与接入平台分别判断。新增厂商原生平台时不能直接复制聚合平台的端点、定价或套餐分类。
2. **确认请求地址与鉴权**：先读完整请求路径，再取适配器需要的 `base_url`；核对是否支持 Bearer 或 `x-api-key`。官方示例有多个鉴权选项时逐个查看，不能只凭默认标签判断不支持；不支持现有鉴权方式的平台不靠新增 JSON 字段伪装支持。
3. **确认按量与套餐**：读取 Key、端点及额度是否互通的官方说明。普通 API 用余额按量扣费才归 `usage`；Token Plan 等套餐独立归 `subscription`，不能因为同一模型名称就合并。
4. **确认型号与协议面**：型号必须是该平台 API 接受的精确 ID。`upstream_protocols` 与 `capabilities` 仅复用同型号既有声明，并与当前平台官方文档核对；没有已验证的行为 profile 就省略。厂商支持原生 Responses 不代表 `openai_compat` 能直接转发它，设备 Responses 仍经 Chat 上游承载。
5. **联动查价**：运行 [价目任务的缺价检查](update-official-pricing.md#先查缺价再按厂商核对)，检查本次涉及的全部型号，包括聚合与套餐里的第三方模型。可核实的模型厂商标准价同步补齐；无可收录价格时保留平台型号并在摘要说明，不能填零价凑齐。

小米 MiMo 的接入与定价入口见 [README](README.md#小米-mimo-官方入口)。按量平台使用 `https://api.xiaomimimo.com/v1` 的 OpenAI Chat 协议面与普通 API Key，官方 API 文档提供 Bearer 鉴权；模型选单收录 `mimo-v2.5-pro` 与 `mimo-v2.5`。更新时重新核对官方页面，不从 Token Plan 的配置样例取按量平台端点。

## 文件形态

顶层：

| 字段 | 取值 |
|---|---|
| `schema` | 恒 `llmgate.platform-models/v2` |
| `version` | 正整数，每次改动加一 |
| `updated_at` | `YYYY-MM-DD` |
| `notes` | 给维护者看的说明；规则修订时同步改 |
| `platforms` | 平台数组，按 按量 → 套餐 → 通用适配 排列 |
| `agents` | 工具订阅数组 |

平台：

| 字段 | 取值 |
|---|---|
| `id` | 平台身份，`[A-Za-z0-9._:/-]`，全文件唯一；内置平台与 `type` 相同 |
| `type` | 固件适配器：`deepseek`、`ark`、`ark_plan`、`qwen_plan`、`opencode_go`、`minimax`、`openai_compat`、`anthropic_compat`。数据新增的平台只能用后两个 |
| `vendor` | 平台名（管理台显示） |
| `base_url` | 固定 HTTPS 端点根，无凭据、查询串与 fragment；OpenAI 兼容通常以 `/v1` 结尾，设备在其后拼 `/chat/completions` 或 `/messages`。内置适配器留空 |
| `custom_base_url` | 只有两条通用兼容适配为 `true` |
| `billing_mode` | `usage` / `subscription` / `none` |
| `suggested_name` | 管理台新建账号时的建议名 |
| `entries` | 入口说明，如「OpenAI 兼容文本入口」 |
| `ability` | 余额 / 额度在哪里看 |
| `source`、`checked_at` | 当次核对的官方页面与日期 |
| `note` | 边界说明：与哪条平台的 Key 不通用、收录了哪类型号 |
| `models` | 选单，`billing_mode: none` 的平台可以为空 |

平台里的模型：

| 字段 | 取值 |
|---|---|
| `name` | 客户端请求里的 `model` 值，也是设备目录里的名字 |
| `kind` | `text` / `video` / `image` |
| `family` | 只有 video / image 写：`ark_video`、`ark_image`、`minimax_video` |
| `upstream_model_id` | 上游真实 ID 与 `name` 不同时才写 |
| `note` | 一句话说明 |
| `upstream_protocols` | 可选，收窄该型号接受的上游协议：`openai_chat`、`openai_responses`、`anthropic_messages` |
| `capabilities` | 可选，固件已实现的行为 profile；只从同型号的既有条目照抄 |

工具订阅（`agents` 段）：

| 字段 | 取值 |
|---|---|
| `provider` | `codex` / `grok` / `claude` / `cursor`，各一条 |
| `vendor`、`source`、`checked_at`、`note` | 同平台 |
| `models[]` | `name`、`kind`（恒 `text`）、`note`，可逐条写 `source` / `checked_at` 覆盖；Cursor 必须有 `pricing` |

## 硬限制

设备解析时整份拒绝超限的文件：平台不超过 64 条，每平台模型不超过 300 条；订阅不超过 16 条，每订阅模型不超过 100 条；文件不超过 256 KiB。同一平台内型号不重名；Codex/Grok/Claude 的型号全局不重名；Cursor 组内不重名，可与其他订阅同名。

Cursor 的 `pricing` 只收 `in`、`out`、`cache_read`、`cache_write`，单位为整数微元 / 百万 token。输入与输出必填，缓存价省略时用输入价，显式 0 免费；美元价使用 `official-pricing.json` 声明的固定汇率。只收官方页面可核实的请求 ID 与标准价，不推断别名、Fast、Auto、长上下文或套餐附加费；未知 ID 的调用照常转发并标为未定价。

## 步骤

1. 看工作树已有改动，读文件 `notes`，确认本次范围、三类的划分与命名约定。
2. 按上面的顺序读取本次平台官方页面；同一页面核对多个型号，缺失信息再查模型页、接入说明或有效公告。
3. 改写既有平台；新增平台放进所属分类的末尾。文本型号只收对话模型；图片、视频型号只在火山方舟与 MiniMax 平台，且必须写 `family`。
4. 检查本次平台型号的厂商价目。涉及 `agents` 时，Codex/Grok/Claude 新型号必须有同名、同 `agent` 的官方价目，否则校验不过；Cursor 只更新本文件内的独立 `pricing`，不添加到共享价目表。
5. 改过的数据文件各自 `version` 加一，`updated_at` 改当天；变更条目更新 `source` 与 `checked_at`，未改条目不刷新日期。
6. 按 [README 校验](README.md#校验) 在实际固件源码目录运行 `catalogcheck -fix` 校验本目录，直到 0 错误；审阅 diff 并执行 `git diff --check`。
7. 输出摘要：新增 / 改价或改端点（含型号变化） / 下架 / 未能核对，每条带出处与核对日期，说明校验结果、剩余缺价及外部 API 是否实际调用过。
