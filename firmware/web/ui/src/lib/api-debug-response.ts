export interface DebugResponse {
  body: string;
  text: string;
  streamStatus?: "completed" | "failed" | "incomplete" | "interrupted";
}

function object(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" ? value as Record<string, unknown> : {};
}

function array(value: unknown): unknown[] { return Array.isArray(value) ? value : []; }

function responseText(value: unknown, protocol: string): string {
  const json = object(value);
  const parts = protocol === "openai_chat" ? array(json.choices).map((choice) => object(object(choice).message).content)
    : protocol === "anthropic_messages" ? array(json.content)
    : array(json.output).flatMap((item) => array(object(item).content));
  return parts.flatMap((part) => {
    const text = typeof part === "string" ? part : object(part).text ?? object(part).refusal;
    return typeof text === "string" ? [text] : [];
  }).join("\n");
}

// Parsing stays in memory. Preserve the full wire response for inspection and
// distinguish a completed stream from a truncated or failed HTTP 200 response.
export function parseDebugResponse(raw: string, contentType: string, protocol: string): DebugResponse {
  if (!contentType.toLowerCase().includes("text/event-stream")) {
    try {
      const json: unknown = JSON.parse(raw);
      return { body: JSON.stringify(json, null, 2), text: responseText(json, protocol) };
    } catch { return { body: raw, text: "" }; }
  }
  const parts = new Map<string, string>();
  let finalText: string | undefined;
  let streamStatus: DebugResponse["streamStatus"] = "interrupted";
  for (const frame of raw.replace(/\r\n?/g, "\n").split("\n\n")) {
    const data = frame.split("\n").filter((line) => line.startsWith("data:")).map((line) => line.slice(5).replace(/^ /, "")).join("\n");
    if (!data || data === "[DONE]") continue;
    try {
      const event = object(JSON.parse(data));
      const key = `${event.output_index ?? 0}:${event.content_index ?? 0}`;
      if (event.type === "response.output_text.delta" && typeof event.delta === "string") {
        parts.set(key, (parts.get(key) ?? "") + event.delta);
      } else if (event.type === "response.output_text.done" && typeof event.text === "string") {
        parts.set(key, event.text);
      } else if (event.type === "response.completed" || event.type === "response.failed" || event.type === "response.incomplete") {
        streamStatus = event.type === "response.completed" ? "completed" : event.type === "response.failed" ? "failed" : "incomplete";
        if (Array.isArray(object(event.response).output)) finalText = responseText(event.response, "openai_responses");
      } else if (event.type === "error") {
        streamStatus = "failed";
      }
    } catch { /* Malformed frames remain visible in the full response. */ }
  }
  return { body: raw, text: finalText || [...parts.values()].join("\n"), streamStatus };
}
