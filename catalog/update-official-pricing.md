# 任务：更新官方目录价

目标：让 `official-pricing.json` 与厂商当前公示的官方按量目录价一致，并把分时段价记进 `schedule`。先读 [AGENTS.md](AGENTS.md)。

## 范围

1. **复核已有条目**：文件里每一条都按其 `source` 打开官方页面，重新核对 `pricing`、`schedule` 与 `note`；页面地址变了就改 `source`。
2. **补齐缺价的型号**：对照 `platform-models.json`，厂商原生平台（`platforms` 段里 `vendor` 就是模型厂商的平台，如 DeepSeek、Kimi、智谱、MiniMax、阿里云百炼的通义千问、OpenAI、Anthropic、Google Gemini、xAI、阶跃星辰、百度千帆的 ERNIE）与 `agents` 段列出的型号，凡厂商公示了按量价而本文件没有的，补一条。
3. **不录**：聚合与转售平台（晨羽AI、硅基流动、OpenRouter、Together、Groq、腾讯 TokenHub / LKEAP，以及百炼、方舟托管的第三方模型）经它们调用的价格；订阅套餐与工具订阅的月费；图片生成、视频生成以外的多模态价（语音、嵌入、重排序）；Cursor 订阅型号的价格（由 `platform-models.json` 的 Cursor 分组维护）。
4. 一个型号只有一条：设备按名不分大小写精确匹配。同一模型的官方别名与带日期 ID 各自成条（如 `claude-haiku-4-5` 与 `claude-haiku-4-5-20251001`）；`agents` 段里有的名字必须逐字节一致。

## 文件形态

顶层：

| 字段 | 取值 |
|---|---|
| `schema` | 恒 `llmgate.official-pricing/v1` |
| `version` | 正整数，每次改动加一 |
| `updated_at` | `YYYY-MM-DD` |
| `currency` / `unit` | 恒 `CNY` / `micro_yuan` |
| `notes` | 给维护者看的说明；规则修订时同步改 |
| `models` | 条目数组 |

条目：

| 字段 | 取值 |
|---|---|
| `name` | 厂商原始模型名，即客户端请求里的 `model` 值 |
| `kind` | `text` / `video` / `image` |
| `vendor` | 模型厂商名 |
| `agent` | 可选，`codex` / `grok` / `claude`：工具订阅代理的主力文本模型，与 `platform-models.json` 的 `agents` 段一一对应 |
| `pricing` | 标准目录价，字段按 `kind`（下表） |
| `schedule` | 可选，分时段价（下节） |
| `source` | 当次核对的官方页面 HTTPS 地址 |
| `checked_at` | 核对日期 `YYYY-MM-DD` |
| `note` | 价签说明：档位口径、长上下文加价、促销、汇率等设备不建模的部分 |

价格字段（整数微元）：

| `kind` | 字段 | 单位 | 说明 |
|---|---|---|---|
| text | `in`、`out` | 微元 / 百万 token | 输入、输出价，必须成对 |
| text | `cache_read` | 微元 / 百万 token | 缓存命中输入价；不写则按 `in` |
| text | `cache_write` | 微元 / 百万 token | 缓存写入价，只在 Anthropic 形口径计费；不写则按 `in` |
| video | `ark_video_token`、`ark_video_token_ref` | 微元 / 百万 token | 火山方舟视频，不含 / 含参考视频输入，必须成对 |
| video | `minimax_video_sec_768p`、`minimax_video_sec_2k` | 微元 / 秒 | MiniMax 视频，按输出分辨率档 |
| video | `minimax_video_image_extra` | 微元 / 张 | 超出免费 5 张的输入图片 |
| video | `minimax_context_ir_in`、`minimax_context_ir_out` | 微元 / 百万 token | MiniMax Context-IR，必须成对 |
| image | `ark_image_each` | 微元 / 张 | 火山方舟图片按张 |
| image | `ark_image_token` | 微元 / 百万 token | 火山方舟图片按 token；与按张只配其一 |

设备只建模这些字段。长上下文加价档、Fast mode 加价、批量与 flex 折扣、分辨率档差价等写进 `note`，`pricing` 记标准档。

### 标准价与分时段价

- `pricing` 是**标准价**：全时段默认、刊例价口径，不折算限时促销、渠道折扣与订阅补贴。厂商分高峰 / 空闲两档时，`pricing` 记高峰（较高）档。
- `schedule` 记厂商按星期与时刻切换的价，例如 DeepSeek：

```json
"schedule": {
  "timezone": "Asia/Shanghai",
  "periods": [
    {
      "label": "工作日空闲时段",
      "days": ["mon", "tue", "wed", "thu", "fri"],
      "hours": [["00:00", "09:00"], ["12:00", "14:00"], ["18:00", "24:00"]],
      "pricing": { "in": 4500000, "cache_read": 150000, "out": 13500000 }
    },
    {
      "label": "周末全天空闲时段",
      "days": ["sat", "sun"],
      "hours": [["00:00", "24:00"]],
      "pricing": { "in": 4500000, "cache_read": 150000, "out": 13500000 }
    }
  ]
}
```

- `timezone` 是厂商计费时区的 IANA 名。`days` 只认 `mon`…`sun`。`hours` 是若干 `[从, 到)` 的 `HH:MM` 区间，「到」可写 `24:00`，跨午夜的时段拆成两段。时段之间不得重叠。每个时段的 `pricing` 字段集必须与标准价相同。不在任何时段内的时刻按标准价。周末全天低价就是 `days` 为 `sat` / `sun`、`hours` 为 `00:00`–`24:00` 的一条。
- 厂商只给折扣比例时，按比例算出每个字段的微元整数，向下取整。
- 厂商没有分时段价时不写 `schedule`。

## 步骤

1. 读文件 `notes`，确认单位、汇率与 `agent` 约定。
2. 按 `vendor` 分组，逐组打开官方定价页。页面是前端渲染而读不出数字时，试厂商文档站的模型页、公告或 API 文档；仍读不出就列为「未能核对」，保留原条目。
3. 逐条核对并改写 `pricing`、`schedule`（有分时段就写，没有就删）、`source`、`checked_at`、`note`；把设备不建模的价签细节写进 `note`。
4. 补新条目；官方已下架的型号删除，但 `agents` 段仍列出的先在摘要里提出，不删。
5. `version` 加一，`updated_at` 改当天。
6. 在固件源码目录执行 `go run ./tools/catalogcheck -fix ../catalog`，直到 0 错误。
7. 输出摘要：新增 / 改价 / 下架 / 未能核对，每条带出处与核对日期。

## 汇率与取整

美元价按 `notes` 里的汇率折算：微元 = 美元价 × 汇率 × 1 000 000，向下取整。汇率只由维护者改 `notes`，本任务不改汇率。
