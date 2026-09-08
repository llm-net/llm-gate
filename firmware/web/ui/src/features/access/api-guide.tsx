// 「使用指南 → API调用」页面：为三个文本协议面提供所选接入路径的配置与示例。
//
// 地址口径不在这里：路径推导、tab 顺序与灯、默认选中规则都在 access.tsx，本文件
// 只负责怎么讲当前这一条——**接入路径由外层的 tab 选，这一层不再自带地址选择器**。
//
// 版面：两栏——左「接入地址」+「使用说明」，右「客户端示例」。配置一个客户端要同时
// 抄两样东西——请求发往哪个地址、示例里怎么写，上下摞着就得来回滚；并排摆一眼全在。
//
// 窄屏落回单列，顺序变成 接入地址 → 使用说明 → 客户端示例——所以**文案一律用卡片名
// 指代同页的卡**（「接入地址」「客户端示例」），不用「左边」「上面」这类方位词：
// 换个宽度就不成立。
//
// 示例里的模型名取当前入口真能调用的第一个，取不到才退回占位符——绝不拿
// OpenAI-only 的模型去教 Anthropic 示例（反之亦然）。
//
// 两个受众共用这一份：管理员（使用指南页，模型清单是全设备的）与 Key 持有者
// （「接入方法」页 /connect，清单按那把 Key 裁剪）。差别只在「使用说明」那张卡的
// 措辞——管理员被指去「API密钥」页签 Key，持有者手里已经有 Key、够不着管理页。

import { Fragment } from "react";

import { Card } from "@/components/ui/card";
import { Sample, type AccessLine, type AccessTarget } from "@/features/access/access";
import * as api from "@/lib/api";
import { sentenceSeparator, t, tx } from "@/lib/i18n";

// sampleModelName 按入口给示例选第一个真能调用的模型；该入口一个都没有才退回
// 占位符。
function sampleModelName(models: api.ServableModel[], protocol: api.Protocol): string {
  return models.find((m) => m.protocols.includes(protocol))?.name ?? t("<模型名>");
}

// ---- 接入地址卡片 ----

function AddrLine({ line, tagged, text }: { line: AccessLine; tagged: boolean; text?: string }) {
  return (
    <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
      {tagged ? <span className="text-muted-foreground shrink-0 text-xs">{line.tag}</span> : null}
      <code className="font-mono text-xs break-all">{text ?? line.base}</code>
      {text === undefined ? <span className="text-muted-foreground text-xs">{line.hint}</span> : null}
    </div>
  );
}

// TargetBody 一条路径的读数：基址、三个协议面的完整调用地址、认证方式、模型列表
// 接口。这条路径不可用时只给一句说明——没有地址可给，列一堆入口是骗人。
function TargetBody({ target }: { target: AccessTarget }) {
  if (target.lines.length === 0) return <p className="text-muted-foreground text-xs">{target.empty}</p>;
  // 只有一行时不挂行前标签：标签是用来区分多行的，只剩一行就成了噪声。
  const tagged = target.lines.length > 1;
  const urls = (method: string, path: string) =>
    target.lines.map((l) => (
      <AddrLine key={l.base} line={l} tagged={tagged} text={`${method} ${l.base}${path}`} />
    ));
  return (
    <dl className="grid grid-cols-1 sm:grid-cols-[11rem_1fr] gap-x-4 gap-y-2 text-sm">
      <dt className="text-muted-foreground">{t("设备地址")}</dt>
      <dd>
        {target.lines.map((l) => (
          <AddrLine key={l.base} line={l} tagged={tagged} />
        ))}
      </dd>
      {api.TextProtocolSurfaces.map((surface) => (
        <Fragment key={surface.protocol}>
          <dt className="text-muted-foreground">{api.protocolLabel(surface.protocol)}</dt>
          <dd>{urls("POST", surface.path)}</dd>
        </Fragment>
      ))}
      <dt className="text-muted-foreground">{t("认证方式")}</dt>
      <dd className="text-xs">
        {tx("请求头 <c>Authorization: Bearer <API密钥></c> 或 <c>x-api-key: <API密钥></c>，两种都接受", {
          c: (s) => <code className="font-mono">{s}</code>,
        })}
      </dd>
      <dt className="text-muted-foreground">{t("模型列表")}</dt>
      <dd>{urls("GET", "/v1/models")}</dd>
    </dl>
  );
}

// SampleBlocks 按当前路径的第一条地址生成示例。这条路径不可用时不编地址，只说
// 一句为什么这里是空的。
function SampleBlocks({
  target,
  openAIModel,
  responsesModel,
  anthropicModel,
}: {
  target: AccessTarget;
  openAIModel: string;
  responsesModel: string;
  anthropicModel: string;
}) {
  const primary = target.lines[0];
  if (primary === undefined) {
    return <p className="text-muted-foreground text-xs">{t("这条接入方式启用后，示例会用它的地址生成。")}</p>;
  }
  const base = primary.base;
  // 每段示例整段是一个键：<API密钥>、<模型名> 占位与「你好」随语言翻，命令与 JSON
  // 结构原样保留；地址与模型名以 {base} / {openAIModel} 占位填入。
  const curl = t(
    `curl {base}/v1/chat/completions \\
  -H "Authorization: Bearer <API密钥>" \\
  -H "Content-Type: application/json" \\
  -d '{"model": "{openAIModel}", "messages": [{"role": "user", "content": "你好"}]}'`,
    { base, openAIModel },
  );
  const openai = t(
    `from openai import OpenAI

client = OpenAI(base_url="{base}/v1", api_key="<API密钥>")
reply = client.chat.completions.create(model="{openAIModel}",
    messages=[{"role": "user", "content": "你好"}])
print(reply.choices[0].message.content)`,
    { base, openAIModel },
  );
  const responses = t(
    `curl {base}/v1/responses \\
  -H "Authorization: Bearer <API密钥>" \\
  -H "Content-Type: application/json" \\
  -d '{"model": "{responsesModel}", "store": false, "input": "你好"}'`,
    { base, responsesModel },
  );
  const anthropic = t(
    `curl {base}/v1/messages \\
  -H "x-api-key: <API密钥>" \\
  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '{"model": "{anthropicModel}", "max_tokens": 256, "messages": [{"role": "user", "content": "你好"}]}'`,
    { base, anthropicModel },
  );
  // 有真实模型名时示例整段复制即可跑，措辞不能再说它是占位符。
  const notes: string[] = [];
  if (target.lines.length > 1)
    notes.push(t("地址用的是「接入地址」里第一条（{base}），换成同组内任一条都等价。", { base }));
  notes.push(
    openAIModel.startsWith("<") && responsesModel.startsWith("<") && anthropicModel.startsWith("<")
      ? t("示例中的 <API密钥>、<模型名> 是占位符，替换成你的真实值。")
      : t("<API密钥> 换成你自己的那把；各示例已分别选用当前入口可调用的模型，找不到时保留 <模型名> 占位符。"),
  );
  return (
    <>
      <p className="text-muted-foreground text-xs">{notes.join(sentenceSeparator())}</p>
      <Sample title={t("curl（OpenAI Chat 协议面）")} text={curl} />
      <Sample title={t("OpenAI SDK（Python · OpenAI Chat 协议面）")} text={openai} />
      <Sample title={t("curl（OpenAI Responses 协议面）")} text={responses} />
      <Sample title={t("curl（Anthropic Messages 协议面）")} text={anthropic} />
    </>
  );
}

// ---- 使用说明 ----

// 接进一个客户端要备齐、要填的那几样，按先后排。
//
// 第 ① 步只指路、**不复述 Key 明文的取用口径**（能取几次、哪些行取不到都归
// 「API密钥」页）：那条口径变过一次（明文封存入库，从签发时一次性展示改成随时
// 可复制），抄在两处必漏一处。这里只保证不再教人「当场抄下来存好」。
//
// 第 ③ 步指的是「接入地址」里那条 GET /v1/models：模型清单本身不在这一页上，
// 端点才是那份真值（持有者视角同页另有「可用模型」卡，那份与端点一致）。
function HowToCard({ audience }: { audience: Audience }) {
  const holder = audience === "holder";
  return (
    <Card className="gap-3 p-6">
      <h2 className="font-semibold">{t("使用说明")}</h2>
      <p className="text-muted-foreground text-xs">
        {t("按客户端使用的协议面选择 OpenAI Chat、OpenAI Responses 或 Anthropic Messages，填入对应地址、API密钥和支持该协议面的模型名。")}
      </p>
      <ol className="list-decimal space-y-1.5 pl-5 text-sm">
        <li>
          {holder
            ? t("你贴入的这把 API 密钥就是凭证，妥善保管、不要贴进不受信任的地方；设备管理员随时可以停用它。")
            : t("在「API密钥」页新建一把密钥；明文随时能回那里点「复制」取回，不必当场抄下来存好。")}
        </li>
        <li>{t("把「接入地址」里的地址和这把 API密钥填进客户端配置——「客户端示例」给的是可以整段抄走的写法。")}</li>
        <li>
          {holder
            ? t(
                "客户端的 model 字段填模型名——下方「可用模型」卡列的就是这把 Key 当前能调的全部模型，与 GET /v1/models 一致；要用别的模型请联系设备管理员。",
              )
            : t(
                "客户端的 model 字段填模型名——「接入地址」里那条 GET /v1/models 列的就是当前能调的全部模型；模型目录在「模型接入」页维护。",
              )}
        </li>
      </ol>
    </Card>
  );
}

/** 受众：管理员（使用指南页）或 Key 持有者（「接入方法」页）。 */
export type Audience = "admin" | "holder";

/** API调用页面正文。target 是页头 tab 选中的那条接入路径。 */
export function ApiGuide({
  target,
  models,
  audience = "admin",
}: {
  target: AccessTarget;
  models: api.ServableModel[];
  audience?: Audience;
}): React.ReactElement {
  return (
    // 两栏；窄屏落回单列，顺序即 DOM 顺序 接入地址 → 使用说明 → 客户端示例。
    <div className="grid gap-4 lg:grid-cols-2 lg:items-start">
      <div className="flex flex-col gap-4">
        <Card className="gap-3 p-6">
          <h2 className="font-semibold">{t("接入地址")}</h2>
          <TargetBody target={target} />
          <p className="text-muted-foreground text-xs">{t("OpenAI Responses 协议面支持无状态文本对话和函数工具调用。请设置 store 为 false，并通过 input 传入完整上下文；不支持 previous_response_id。")}</p>
        </Card>
        <HowToCard audience={audience} />
      </div>
      <Card className="gap-3 p-6">
        <h2 className="font-semibold">{t("客户端示例")}</h2>
        <SampleBlocks
          target={target}
          openAIModel={sampleModelName(models, "openai_chat")}
          responsesModel={sampleModelName(models, "openai_responses")}
          anthropicModel={sampleModelName(models, "anthropic_messages")}
        />
      </Card>
    </div>
  );
}
