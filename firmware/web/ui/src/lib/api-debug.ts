import type { Protocol } from "@/lib/api";
import { keyRequest, request } from "@/lib/api/client";
import { ApiError } from "@/lib/api/client";
import { currentLang, t } from "@/lib/i18n";
import { parseDebugResponse, type DebugResponse } from "@/lib/api-debug-response";

export interface DebugModel { name: string; provider?: "codex" | "grok"; protocols: Protocol[]; file_types: Partial<Record<Protocol, string[]>> }
export interface DebugInput {
  model: string;
  provider?: DebugModel["provider"];
  protocol: Protocol;
  prompt: string;
  max_tokens: number;
  files: { name: string; type: string; data: string }[];
}
export interface DebugResult extends DebugResponse { status: number; elapsed: number; requestID: string }
export interface DebugClient {
  models: (keyID: number) => Promise<{ models: DebugModel[] }>;
  submit: (keyID: number, input: DebugInput, signal: AbortSignal) => Promise<DebugResult>;
}

// Keys remain in the holder component's closure. Requests never carry cookies
// in holder mode; admin mode sends only the selected row ID, never plaintext.
export function debugClient(holderKey?: string): DebugClient {
  return {
    models: (keyID) => holderKey === undefined
      ? request("GET", `/admin/v1/api-debug/models?key_id=${keyID}`)
      : keyRequest("/gate-helper/v1/api-debug/models", holderKey),
    async submit(keyID, input, signal) {
      const headers: Record<string, string> = { "Content-Type": "application/json", "Accept-Language": currentLang() };
      if (holderKey === undefined) headers["X-LlmGate-CSRF"] = "1";
      else headers.Authorization = `Bearer ${holderKey}`;
      const start = performance.now();
      let response: Response;
      try {
        response = await fetch(holderKey === undefined ? `/admin/v1/api-debug/${keyID}` : "/gate-helper/v1/api-debug", {
          method: "POST", headers, credentials: holderKey === undefined ? "same-origin" : "omit",
          cache: "no-store", signal, body: JSON.stringify(input),
        });
        const raw = await response.text();
        return {
          status: response.status, elapsed: Math.round(performance.now() - start), requestID: response.headers.get("X-Request-Id") ?? "",
          ...parseDebugResponse(raw, response.headers.get("Content-Type") ?? "", input.protocol),
        };
      } catch (err) {
        if (signal.aborted) throw err;
        throw new ApiError(0, "network", t("无法连接服务器，请检查设备与网络后重试"));
      }
    },
  };
}
