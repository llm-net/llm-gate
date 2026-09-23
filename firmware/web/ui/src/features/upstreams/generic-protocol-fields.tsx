import { SystemOneTestQuestions } from "./systemone-test-questions";
import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";

const protocols: api.Protocol[] = ["openai_chat", "openai_responses", "anthropic_messages", "systemone"];

export function GenericProtocolFields({ urls, onChange, apiKey, upstreamID, egressMode }: {
  urls: api.ProtocolURLs;
  onChange: (urls: api.ProtocolURLs) => void;
  apiKey: string;
  upstreamID?: number | undefined;
  egressMode?: api.EgressMode | undefined;
}) {
  return <fieldset className="flex flex-col gap-2">
    <legend className="mb-2 text-sm font-medium">{t("协议面")}</legend>
    <div className="grid gap-3 sm:grid-cols-2">
      {protocols.map((protocol) => <div key={protocol} className="rounded-lg border p-3">
        <label className="flex items-center gap-2 text-sm font-medium">
          <Checkbox checked={urls[protocol] !== undefined} onCheckedChange={(checked) => {
            const next = { ...urls };
            if (checked === true) next[protocol] = "";
            else delete next[protocol];
            onChange(next);
          }} />
          {api.protocolLabel(protocol)}
        </label>
        {urls[protocol] !== undefined ? <ProtocolEndpoint
          protocol={protocol} url={urls[protocol]} onURLChange={(url) => onChange({ ...urls, [protocol]: url })}
          apiKey={apiKey} upstreamID={upstreamID} egressMode={egressMode}
        /> : null}
      </div>)}
    </div>
    <p className="text-muted-foreground text-xs">{t("至少选择一个协议面，勾选后必须填写地址。测试模型名仅用于测试，不会添加模型；测试会发起一次上游请求。")}</p>
  </fieldset>;
}

function ProtocolEndpoint({ protocol, url, onURLChange, apiKey, upstreamID, egressMode }: {
  protocol: api.Protocol;
  url: string;
  onURLChange: (url: string) => void;
  apiKey: string;
  upstreamID?: number | undefined;
  egressMode?: api.EgressMode | undefined;
}) {
  const [model, setModel] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<api.SourceTestResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const generation = useRef(0);
  useEffect(() => {
    generation.current++;
    setResult(null); setError(null); setBusy(false);
    return () => { generation.current++; };
  }, [url, model, apiKey, upstreamID, egressMode]);
  async function test() {
    const current = ++generation.current;
    setBusy(true); setResult(null); setError(null);
    try {
      const res = await api.testUpstreamProtocol({ protocol, base_url: url.trim(), api_key: apiKey, model: model.trim(), upstream_id: upstreamID, egress_mode: egressMode });
      if (current === generation.current) setResult(res);
    } catch (err) {
      if (current === generation.current) setError(api.errorMessage(err));
    } finally {
      if (current === generation.current) setBusy(false);
    }
  }
  return <div className="mt-2 flex flex-col gap-2">
    <Input required type="url" aria-label={t("{protocol} 服务地址", { protocol: api.protocolLabel(protocol) })}
      value={url} onChange={(e) => onURLChange(e.target.value)} autoComplete="off"
      placeholder={protocol === "systemone" ? "http://192.168.1.30:18080" : "https://api.example.com/v1"} />
    <div className="flex gap-2">
      <Input aria-label={t("{protocol} 测试模型名", { protocol: api.protocolLabel(protocol) })}
        value={model} onChange={(e) => setModel(e.target.value)} autoComplete="off" placeholder={t("测试模型名")} />
      <Button type="button" variant="outline" disabled={busy || !url.trim() || !model.trim() || (!apiKey && !upstreamID)} onClick={() => { void test(); }}>
        {busy ? t("测试中…") : t("测试")}
      </Button>
    </div>
    <p className="text-muted-foreground text-xs">{protocol === "systemone" ? t("服务根地址，自动追加 /v1/systemone；一次请求同时测试 Choice、Noul、Score。") : t("端点根地址（通常包含 /v1），自动追加协议路径。")}</p>
    <div aria-live="polite" className="text-xs">
      {result ? <p className={result.ok ? "text-emerald-600" : "text-destructive"}>
        {result.ok ? t("测试通过 · {ms} ms", { ms: result.latency_ms }) : t("测试失败：{message}", { message: result.message })}
      </p> : null}
      {result ? <SystemOneTestQuestions questions={result.questions} /> : null}
      {error ? <p className="text-destructive">{error}</p> : null}
    </div>
  </div>;
}
