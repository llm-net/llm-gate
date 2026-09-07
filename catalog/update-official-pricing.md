# 任务：更新官方目录价

目标：让 `official-pricing.json` 与厂商当前公示的官方按量目录价一致，并把分时段价记进 `schedule`。先读 [AGENTS.md](AGENTS.md)。

## 范围

1. **复核已有条目**：本次范围内每条按其 `source` 打开官方页面，重新核对 `pricing`、`schedule` 与 `note`；页面地址变了就改 `source`。用户指定厂商或型号时只核对指定范围及必要联动，完整更新才覆盖全部条目。
2. **补齐缺价的型号**：对照 `platform-models.json` 的全部 `platforms[].models` 与 Codex/Grok/Claude 的 `agents[].models`，凡模型厂商公示了可收录的按量价而本文件没有的，补一条。候选包括聚合、转售、套餐与通用适配里的型号；不能因为没有厂商原生平台条目就跳过。`platforms[].vendor` 是接入平台名，须另外确认模型厂商，例如 MiMo 查小米、Kimi 查月之暗面。
3. **不录**：聚合与转售平台（晨羽AI、硅基流动、OpenRouter、Together、Groq、腾讯 TokenHub / LKEAP，以及百炼、方舟托管的第三方模型）经它们调用的价格；订阅套餐与工具订阅的月费；图片生成、视频生成以外的多模态价（语音、嵌入、重排序）；Cursor 订阅型号的价格（由 `platform-models.json` 的 Cursor 分组维护）。
4. 一个型号只有一条：设备按名不分大小写精确匹配。同一模型的官方别名与带日期 ID 各自成条（如 `claude-haiku-4-5` 与 `claude-haiku-4-5-20251001`）；`agents` 段里有的名字必须逐字节一致。

## 先查缺价，再按厂商核对

在 `catalog/` 执行以下只读命令，需要 Node.js。它按完整名称找缺价与大小写差异，并列出所在平台及 `upstream_model_id`，不修改文件、不去掉组织前缀、不推断别名：

```sh
node --input-type=module <<'JS'
import { readFileSync } from 'node:fs';
const read = (name) => JSON.parse(readFileSync(name, 'utf8'));
const pricing = new Map(read('official-pricing.json').models.map((m) => [m.name.toLowerCase(), m]));
const catalog = read('platform-models.json');
const pending = new Map();
function inspect(model, location) {
  const price = pricing.get(model.name.toLowerCase());
  if (price?.name === model.name) return;
  const row = pending.get(model.name) ?? {
    name: model.name,
    status: price ? '大小写待核对' : '缺价候选',
    pricing_name: price?.name,
    locations: [],
  };
  row.locations.push({ location, upstream_model_id: model.upstream_model_id });
  pending.set(model.name, row);
}
for (const platform of catalog.platforms) {
  for (const model of platform.models) inspect(model, `platform:${platform.id}`);
}
for (const agent of catalog.agents) {
  if (agent.provider === 'cursor') continue;
  for (const model of agent.models) inspect(model, `agent:${agent.provider}`);
}
console.log(JSON.stringify([...pending.values()], null, 2));
JS
```

按任务范围筛选输出，再按**模型厂商**归组；相同官方价目页面只需打开一次。每个候选都要归入「补价」「已有同名价但需核对拼写」「不收录并说明原因」或「未能核对」。平台特有的命名空间、路由别名不自动继承厂商型号的价格；先用官方文档确认身份，只有厂商原始型号或官方别名进入价目表，不为消除候选而录入平台自定义名字。

本命令仅做名称对照；`catalogcheck` 也不验证全部普通平台型号的价格覆盖率或网页数字。命令无输出、校验通过均不能代替对现有价格的官方核对。

## 价格口径核对

- 优先读取现有 `source` 与 [README 官方入口](README.md#小米-mimo-官方入口)。搜索只用于寻找官方页面；页面动态渲染时检查表格标签、国内/海外切换与鉴权示例选项，再试同厂商模型页或有效公告。
- **地域与币种**：有国内人民币标准价时直接使用；不要拿海外美元价按固定汇率折算成国内价。只有美元价的条目才按 `notes` 换算，并在 `note` 写清地域、原币种和汇率。
- **列名与单位**：`in` 对应未命中缓存的输入，`cache_read` 对应缓存命中输入，`out` 对应输出；`cache_write` 是独立缓存写入价。先确认每百万、每千、每次或每秒的单位，不能按网页列顺序盲填，也不能把缓存命中价当作写入价。
- **标准价与优惠**：官方已生效的永久调价属于标准价；限时促销、赠送额度和套餐 Credits 不折算进 `pricing`。只公示「缓存写入限时免费」而没有独立标准价时不填 `cache_write: 0`，在 `note` 说明，不能把暂时免费视为长期价。
- **档位与附加费**：确认是否按输入长度、时段、模式分档；只保留当前仍有效的档位说明。联网搜索等按次费用不混入 token 价，没有受支持字段的费用写在 `note`。
- **证据冲突**：页面读不到数字、地域不明或定价页与公告冲突未能厘清时，保留原值并列为「未能核对」；缺价条目保持缺价，不用零元代替未知。

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

1. 看工作树已有改动，读文件 `notes`，确认本次范围、单位、汇率与 `agent` 约定；运行上面的缺价检查。
2. 合并本次现有价目与缺价候选，按模型厂商归组；按上面的口径读取官方页面并逐项核对。
3. 只对有证据的变化改写 `pricing`、`schedule`、`source`、`checked_at`、`note`；确认取消分时段价后才删除 `schedule`，未改条目不刷新日期。
4. 补新条目；官方已下架的型号删除，但 `agents` 段仍列出的先在摘要里提出，不删。重跑缺价检查，对本次范围内剩余候选逐项说明原因。
5. 改过的数据文件 `version` 加一，`updated_at` 改当天；未改文件不动版本。
6. 按 [README 校验](README.md#校验) 在实际固件源码目录运行 `catalogcheck -fix` 校验本目录，直到 0 错误；审阅 diff 并执行 `git diff --check`。
7. 输出摘要：新增 / 改价或改端点 / 下架 / 未能核对，每条带出处与核对日期，说明校验结果与剩余缺价原因。

## 汇率与取整

美元价按 `notes` 里的汇率折算：微元 = 美元价 × 汇率 × 1 000 000，向下取整。使用十进制定点或整数运算，避免二进制浮点造成微元误差；核对原始单位后再换算。汇率只由维护者改 `notes`，本任务不改汇率。
