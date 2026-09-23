// node --experimental-strip-types --test scripts/api-debug-response.test.mjs
import assert from "node:assert/strict";
import test from "node:test";
import { parseDebugResponse } from "../src/lib/api-debug-response.ts";

const sse = (events) => events.map((event) => `data: ${JSON.stringify(event)}\n\n`).join("");
const output = (text) => [{ type: "message", content: [{ type: "output_text", text }] }];

test("extracts text from all three JSON protocols and retains non-JSON errors", () => {
  for (const [protocol, payload] of [
    ["openai_chat", { choices: [{ message: { content: "hello" } }] }],
    ["anthropic_messages", { content: [{ type: "text", text: "hello" }] }],
    ["openai_responses", { output: output("hello") }],
  ]) {
    const parsed = parseDebugResponse(JSON.stringify(payload), "application/json", protocol);
    assert.equal(parsed.text, "hello");
    assert.equal(parsed.streamStatus, undefined);
    assert.deepEqual(JSON.parse(parsed.body), payload);
  }
  assert.deepEqual(parseDebugResponse("upstream unavailable", "text/plain", "openai_responses"), { body: "upstream unavailable", text: "" });
});

test("Codex completed SSE shows final text once and preserves full wire response", () => {
  const raw = ": heartbeat\n\n" + sse([
    { type: "response.output_text.delta", delta: "hel" },
    { type: "response.output_text.delta", delta: "lo" },
    { type: "response.output_text.done", text: "hello" },
    { type: "response.completed", response: { output: output("hello") } },
  ]) + "data: [DONE]\n\n";
  const parsed = parseDebugResponse(raw.replaceAll("\n", "\r\n"), "text/event-stream; charset=utf-8", "openai_responses");
  assert.equal(parsed.text, "hello");
  assert.equal(parsed.streamStatus, "completed");
  assert.equal(parsed.body, raw.replaceAll("\n", "\r\n"));
});

test("multiline SSE data and multiple content parts retain complete output", () => {
  const raw = sse([
    { type: "response.output_text.delta", output_index: 0, delta: "first" },
    { type: "response.output_text.done", output_index: 1, text: "second" },
  ]) + 'event: response.completed\ndata: {"type":"response.completed",\ndata: "response":{}}\n\n';
  const parsed = parseDebugResponse(raw, "text/event-stream", "openai_responses");
  assert.equal(parsed.text, "first\nsecond");
  assert.equal(parsed.streamStatus, "completed");
});

test("failed, incomplete, and truncated HTTP 200 streams are not successful results", () => {
  for (const status of ["failed", "incomplete", "interrupted"]) {
    const raw = sse([
      { type: "response.output_text.delta", delta: "partial" },
      ...(status === "interrupted" ? [] : [{ type: `response.${status}`, response: { error: { message: "failure" } } }]),
    ]) + "data: [DONE]\n\n";
    const parsed = parseDebugResponse(raw, "text/event-stream", "openai_responses");
    assert.equal(parsed.text, "partial");
    assert.equal(parsed.streamStatus, status);
  }
  assert.equal(parseDebugResponse(sse([{ type: "error", message: "failure" }]), "text/event-stream", "openai_responses").streamStatus, "failed");
});
