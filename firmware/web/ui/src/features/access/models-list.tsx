// 「可用模型」卡片：这把 Key 此刻真能调用的模型清单（对话模型 + 视频/图像模型）。
//
// 挂在「接入方法」页（/connect）的API调用标签下：Key 持有者够不着模型目录端点，
// 这份清单是他们唯一能自助看到可用模型的地方。读数口径与数据面 GET /v1/models
// 同一份（模型、来源、上游三者都启用、且在这把 Key 的可用模型范围内），随接入
// 读数一次取回，改动下一个请求即生效。
//
// 空清单的措辞按受众分：管理员被指去「模型接入」页加模型，持有者只能找管理员。

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import * as api from "@/lib/api";
import { t, tx } from "@/lib/i18n";

// aigcAPILabel 是协议面标识的展示名：已知的收敛到 protocolLabel（一物一名，单点
// 定义在 lib/api）；endpoint 的 api 字段是 string——未来新协议面会先出现在服务端
// ——未知值原样透出而不是编一个。
function aigcAPILabel(apiName: string): string {
  switch (apiName) {
    case "minimax_video":
    case "ark_video":
    case "ark_image":
      return api.protocolLabel(apiName);
    default:
      return apiName;
  }
}

function EntryBadges({ m }: { m: api.ServableModel }) {
  if (m.protocols.length === 0) {
    return (
      <Badge
        variant="outline"
        className="text-signal-alert"
        title={t("该模型挂的上游当前不服务任何协议面，调用会被拒绝——请管理员检查上游类型")}
      >
        {t("无可用协议面")}
      </Badge>
    );
  }
  return (
    <div className="flex flex-wrap gap-1">
      {m.protocols.map((p) => (
        <Badge key={p} variant="secondary">
          {api.protocolLabel(p)}
        </Badge>
      ))}
    </div>
  );
}

// AIGC 小节。每个视频 / 图像模型固定一个厂商协议面（源头厂商官方接口），设备原样
// 转发——所以这里直接标出协议面并教官方路径，客户端按该厂商的官方文档构造请求
// 即可，只把地址换成「本设备 + /<厂商段>」、认证换成本设备签发的 API密钥。
function AigcSection({ models }: { models: api.AIGCModel[] }) {
  const has = (a: string) => models.some((m) => m.api === a);
  return (
    <div className="flex flex-col gap-2">
      <h3 className="text-sm font-medium">{t("视频 / 图像模型")}</h3>
      <p className="text-muted-foreground text-xs">
        {t("这些模型不在上面的对话清单里，走各自厂商的协议面：接入地址后面加厂商段（/minimax、/ark），再接厂商官方路径。认证同上，模型名原样填进 model 字段。")}
      </p>
      {has("minimax_video") ? (
        <p className="text-muted-foreground text-xs">
          {tx(
            "MiniMax 视频协议面（与官方文档完全一致，设备原样转发；把 MiniMax SDK 的 base_url 换成「上面的接入地址 + /minimax」）：<c>POST /minimax/v2/video_generation</c> 提交、<c>GET /minimax/v2/query/video_generation/{task_id}</c> 查询（成片地址在查询响应里，链接失效就重查）、<c>DELETE /minimax/v2/video_generation/{task_id}</c> 取消；提示词增强走 <c>POST /minimax/v2/h3_context_ir</c>。素材用公网 URL 或 base64 data URI（设备不代理素材上传）。",
            { c: (s) => <code>{s}</code> },
          )}
        </p>
      ) : null}
      {has("ark_video") ? (
        <p className="text-muted-foreground text-xs">
          {tx(
            "火山方舟 视频协议面（与官方文档完全一致，设备原样转发；把方舟 SDK 的 base_url 换成「上面的接入地址 + /ark/api/v3」）：<c>POST /ark/api/v3/contents/generations/tasks</c> 提交、<c>GET /ark/api/v3/contents/generations/tasks/{id}</c> 查询（成片地址在查询响应里，24 小时有效）、<c>DELETE /ark/api/v3/contents/generations/tasks/{id}</c> 取消或删记录。素材用公网 URL 或 base64 data URI（设备不代理素材上传）。",
            { c: (s) => <code>{s}</code> },
          )}
        </p>
      ) : null}
      {has("ark_image") ? (
        <p className="text-muted-foreground text-xs">
          {tx(
            "火山方舟 图像协议面（同步返回，与官方文档完全一致；base_url 同样换成「上面的接入地址 + /ark/api/v3」）：<c>POST /ark/api/v3/images/generations</c>。必填 model 与 prompt；图像地址 24 小时有效，请及时下载或转存。",
            { c: (s) => <code>{s}</code> },
          )}
        </p>
      ) : null}
      <div className="w-full overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("模型名")}</TableHead>
              <TableHead>{t("种类与协议面")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {models.map((m) => (
              <TableRow key={m.name}>
                <TableCell>
                  <code className="font-mono text-xs">{m.name}</code>
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap gap-1">
                    <Badge variant="outline">{api.kindLabel(m.kind)}</Badge>
                    {m.available && m.api !== undefined ? (
                      <Badge variant="secondary" title={t("该模型固定走这一个厂商协议面，请求按该厂商官方文档构造")}>
                        {aigcAPILabel(m.api)}
                      </Badge>
                    ) : (
                      <Badge
                        variant="outline"
                        className="text-signal-alert"
                        title={t("该模型挂的上游都不服务此种类的任何协议面，提交会被拒绝——请管理员在「模型接入」页检查它挂的上游")}
                      >
                        {t("无可用协议面")}
                      </Badge>
                    )}
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

/** 可用模型卡片：对话模型表 + 视频/图像模型小节，两者都空时只给一句说明。 */
export function ModelsCard({
  models,
  aigc,
  audience = "admin",
}: {
  models: api.ServableModel[];
  aigc: api.AIGCModel[];
  audience?: "admin" | "holder";
}): React.ReactElement {
  const holder = audience === "holder";
  if (models.length === 0 && aigc.length === 0) {
    return (
      <Card className="gap-3 p-6">
        <h2 className="font-semibold">{t("可用模型")}</h2>
        <p className="text-muted-foreground text-xs">
          {holder
            ? t("这把 Key 当前没有可调用的模型。请联系设备管理员在「API密钥 → 可用模型」中授权。")
            : t("当前没有可用模型。到「模型接入」页添加模型并挂上启用的上游，这里就会列出来。")}
        </p>
      </Card>
    );
  }
  return (
    <Card className="gap-3 p-6">
      <div className="flex items-center gap-3">
        <h2 className="font-semibold">{t("可用模型")}</h2>
        <Badge variant="secondary" title={t("共 {n} 个", { n: models.length + aigc.length })}>
          {models.length + aigc.length}
        </Badge>
      </div>
      {models.length > 0 ? (
        <>
          <p className="text-muted-foreground text-xs">
            {t("把「模型名」原样填进客户端的 model 字段（区分大小写）。这份清单与调用 GET /v1/models 拿到的完全一致。")}
          </p>
          <div className="w-full overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("模型名")}</TableHead>
                  <TableHead>{t("可用协议面")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {models.map((m) => (
                  <TableRow key={m.name}>
                    <TableCell>
                      <code className="font-mono text-xs">{m.name}</code>
                    </TableCell>
                    <TableCell>
                      <EntryBadges m={m} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </>
      ) : (
        <p className="text-muted-foreground text-xs">
          {holder
            ? t("这把 Key 当前没有可调用的对话模型。")
            : t("当前没有可用的对话模型。到「模型接入」页添加模型并挂上启用的上游，这里就会列出来。")}
        </p>
      )}
      {aigc.length > 0 ? <AigcSection models={aigc} /> : null}
    </Card>
  );
}
