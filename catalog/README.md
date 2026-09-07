# LLM Gate 模型目录数据

本目录是 LLM Gate 两份公开数据文件的维护源。设备通过「数据升级」从官网匿名下载它们，官网不可达时使用固件内嵌的基线。

| 文件 | 记什么 | 设备怎么用 |
|---|---|---|
| [`official-pricing.json`](official-pricing.json) | 官方目录价：一个模型多少钱，含分时段价 | 数据升级后为设备上「未定价」的模型填目录价；Codex/Grok/Claude 订阅按其中带 `agent` 标记的条目记名义金额；Cursor 的独立订阅价格在平台模型文件维护 |
| [`platform-models.json`](platform-models.json) | 平台模型信息：哪个平台有哪些模型 | 管理台「模型接入」三个页面的选单，以及工具订阅的权威模型清单 |

官网 `https://llm.net/updates/data/` 下的同名文件与固件内嵌的 `platform-models.json` 都是本目录文件的逐字节副本。**改动只在本目录做**，副本由维护者同步。

## 三类模型接入

平台模型信息按设备管理台「模型接入」的三个页面分类，三类都要维护：

| 管理台页面 | 文件里的位置 | 含义 |
|---|---|---|
| API 按量计费 | `platforms` 段，`billing_mode: usage` | 持按量付费 Key 的平台，按 token / 按次 / 按秒计费 |
| API 订阅套餐 | `platforms` 段，`billing_mode: subscription` | 已付费套餐专用的 Key 与端点（火山方舟 Coding Plan、通义千问 Token Plan、OpenCode Go、GLM Coding Plan、Kimi Code 会员等），调度时先用满 |
| 工具订阅 | `agents` 段 | Codex、Grok Build、Claude Code 的文本模型由设备建行；Cursor 独立维护型号价格，四组均只读展示 |

`billing_mode: none` 只给两条通用兼容适配（`openai_compat` / `anthropic_compat`），服务地址由管理员自填。`platforms` 段按 按量 → 套餐 → 通用适配 排列，组内先后就是管理台的展示顺序。

官方目录价记的是模型**厂商**的按量目录价：`pricing` 是全时段默认的标准价，`schedule` 段记厂商按星期与时刻切换的分时段价（例如 DeepSeek 的高峰 / 空闲时段与周末全天空闲价）。字段说明见两份任务文档。

## 维护方式

数据由开发工具按任务文档查询厂商官方页面并改写，人只审阅 diff：

| 任务 | 文档 |
|---|---|
| 更新官方目录价 | [update-official-pricing.md](update-official-pricing.md) |
| 更新平台模型信息 | [update-platform-models.md](update-platform-models.md) |

缺省用 Grok Build，也可用 Codex 或 Claude Code；在本目录启动时都要遵守 [AGENTS.md](AGENTS.md)，[CLAUDE.md](CLAUDE.md) 是它的相对软链接。在本目录启动工具，把任务文档交给它：

```sh
cd catalog
grok      # 缺省。进入后输入：阅读 AGENTS.md，按 update-official-pricing.md 执行
codex     # 备选，同样的指令
```

限定更新可直接交代：「按 update-official-pricing.md 核对小米 MiMo 的指定型号，先查缺价，再读小米官方人民币按量价；补齐后校验并报告 diff。」新增接入平台时同时执行平台任务，并检查这些型号的官方价是否齐全。

## 快速核对

1. 从价目任务的缺价检查命令开始。候选覆盖按量、套餐、聚合及通用适配清单；按模型厂商查价，不按平台名过滤。
2. 优先打开条目 `source`，同一官方页面只读一次、核对多个型号。新增型号先找模型厂商的定价页，平台端点则查接入平台自己的 API 文档。
3. 页面中确认完整型号、国内或海外、人民币或美元、每百万或每千 token、输入与缓存列、输出列、标准价或促销。记录整数微元，文档只保存查询入口与口径。
4. 对本次候选逐项给出结果，再运行结构校验。剩余候选可能是平台别名、无厂商公开按量价或未支持的计费形态，必须核实原因，不能直接填转售价或猜成零元。

### 小米 MiMo 官方入口

| 核对内容 | 官方页面 | 使用方式 |
|---|---|---|
| 按量价格 | [API 定价](https://mimo.mi.com/docs/zh-CN/price/pay-as-you-go) | 使用国内人民币标准价；按表头分别取输入未命中、输入命中缓存、输出 |
| 端点、鉴权与型号 | [OpenAI Chat API](https://mimo.mi.com/docs/zh-CN/api/chat/openai-api) | 核对固定端点、Bearer 鉴权选项及精确模型 ID |
| 普通 API 接入 | [首次调用 API](https://mimo.mi.com/docs/zh-CN/quick-start/summary/first-api-call) | 与套餐接入说明分开阅读，不能拿套餐 Key 或端点配置按量平台 |

`mimo-v2.5` 与 `mimo-v2.5-pro` 的价格都查小米；它们出现在其他平台选单时仍遵循此规则。官方链接跳转时确认最终站点归属，`source` 记录实际读取且支持该条目的官方页面。其他厂商从 JSON 现有 `source` 定位，避免在文档重复维护整份网址清单。

## 校验

两份文件必须通过校验工具才能发布。工具位于固件源码的 `tools/catalogcheck`；公开检出可能只含目录数据，先确认实际路径。若发布仓库根已有完整 `firmware/`，从发布仓库根运行：

```sh
cd firmware
go run ./tools/catalogcheck ../catalog          # 校验：形态、字段、出处、分时段价、两份文件互相对得上
go run ./tools/catalogcheck -fix ../catalog     # 先把两份文件重排成规范格式，再校验
```

若固件源码在其他位置，在 `catalog/` 中保存目录的绝对路径，下面的 `/absolute/path/to/firmware` 替换为实际路径：

```sh
catalog_dir="$(pwd -P)"
(
  cd /absolute/path/to/firmware &&
  go run ./tools/catalogcheck -fix "$catalog_dir"
)
git diff --check -- .
test -L CLAUDE.md && test "$(readlink CLAUDE.md)" = AGENTS.md
```

校验工具不联网查官方价，也不检查全部普通平台型号是否缺价，缺价候选必须另行核对。找不到固件源码时如实报告未校验，不能用 JSON 能解析代替验收。

## 收录纪律

- 每条平台、订阅与价目都记当次核对的官方页面 `source` 与核对日期 `checked_at`；没有出处的条目不收。第三方博客、聚合站与社区帖子不算出处。
- 只描述固件已经实现的形态：平台只能复用内置适配器并给固定 HTTPS 端点，模型能力只能引用固件已有的 profile，价格字段只用固件认识的字段名。数据文件不下发代码。
- 名称逐字节：模型 `name` 是客户端请求里的 `model` 值，按厂商文档原样书写；两份文件里同一个型号写法必须相同。
- 金额一律整数微元（1 元 = 1 000 000 微元）；美元价按价目文件 `notes` 里的固定汇率折算。
- 改动后文件级 `version` 加一、`updated_at` 改为当天；同一版本不改内容。文件保持规范格式：2 空格缩进、每个元素各占一行、UTF-8、末尾一个换行。

## 发布

维护者审阅 diff 后在本仓库提交本目录；官网与固件随后同步这两份文件，设备在下一次数据升级取到新版本。固件内嵌的基线随下一次固件发布更新。
