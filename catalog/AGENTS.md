# catalog — 开发工具工作规则

本目录维护 LLM Gate 的官方目录价（`official-pricing.json`）与平台模型信息（`platform-models.json`）。在这里工作的开发工具（缺省 Grok Build，也可以是 Codex）只做一种事：按任务文档查询厂商官方页面、改写两份 JSON、跑校验、给出变更摘要。任务文档是 [update-official-pricing.md](update-official-pricing.md) 与 [update-platform-models.md](update-platform-models.md)，背景见 [README.md](README.md)。

## 边界

- 只改 `official-pricing.json` 与 `platform-models.json`。不改固件源码，不新建文件，不留笔记或临时文件。
- 只用厂商官方页面作依据：官方文档站、定价页、控制台公告、官方帮助中心。第三方博客、聚合站、社区帖子、搜索摘要都不算出处。官方页面打不开或读不出数字时保留原条目，在摘要里列为「未能核对」；不猜、不编、不按记忆填。
- 不需要登录的页面才是可用出处。不写入任何 API Key、账号、邀请码或个人信息。
- 金额只用整数微元（1 元 = 1 000 000 微元），不出现小数；美元价按价目文件 `notes` 里的固定汇率折算后取整。
- 每条改动都更新 `source` 与 `checked_at`（当天，`YYYY-MM-DD`）；每份改过的文件 `version` 加一、`updated_at` 改当天。没改的条目不动 `checked_at`。
- 名称逐字节：模型 `name` 按厂商文档原样书写，区分大小写；两份文件里同一个型号写法必须相同。
- 只描述固件已经实现的形态。平台的 `type` 只能是文件里已出现的适配器；数据新增的平台只能用 `openai_compat` / `anthropic_compat` 并给固定 HTTPS `base_url`。模型的 `capabilities`、`upstream_protocols` 只从同型号的既有条目照抄，不发明新 profile。价格字段只用价目文件 `notes` 列出的字段名。
- 不删除仍在售的条目。厂商明确下架或文档已移除的型号才删，并在摘要里说明出处。

## 完成前必须

1. 在固件源码目录执行 `go run ./tools/catalogcheck -fix ../catalog`，0 错误（提示可以有）。本仓库的 `firmware/` 就是固件源码目录。
2. 摘要按「新增 / 改价或改端点 / 下架 / 未能核对」四组列出，每条带出处链接与核对日期。
3. 不提交、不推送；由维护者审阅 diff。
