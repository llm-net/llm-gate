#!/usr/bin/env bash
# scripts/smoke.sh — 网关冒烟：构建并拉起 mock-openai 与 gatewayd，验证十八例。
#
# 模型名即上游原始名（iteration-5）：客户端请求什么名字，响应 model 就是什么
# 名字；同一模型可挂多个来源，按优先级选路、未提交即自动故障切换。
#
# chat 入口（openai 协议）：
#   1) 非流式透传（200，未知字段双向透传，响应 model = 请求名）
#   2) SSE（网关注入 include_usage → usage 终块与 [DONE] 到达客户端，
#      chunk 的 model = 请求名）
#   3) 无 Key → 401 missing_api_key（OpenAI 风格错误体）
#   4) 未知模型 → 404 model_not_found
#
# messages 入口（anthropic 协议）：
#   5) 非流式透传（200，x-api-key 鉴权，未知字段双向透传，model = 请求名）
#   6) SSE（事件序列完整 message_start→ping→content_block_*→message_delta→
#      message_stop，message_start 的 message.model = 请求名）
#   7) 无 Key → 401（Anthropic 风格 {"type":"error","error":{...}}）
#   8) count_tokens 透传（200 {"input_tokens":N}）
#   9) count_tokens 上游 404 → 网关本地粗估兜底（200 + 日志 fallback_heuristic）
#
# 动态路由（iteration-5）：
#  10) 双入口同名模型：同一个 mock-gpt-4o-mini 两个入口都 200
#  11) 协议过滤：只挂 ark 型来源的模型进 /v1/messages → 404 指向 chat 入口
#      （ark 无内置 anthropic 端点；判定不拨号，真实上游不会被触达）
#  12) 优先级故障切换：高优先级来源指死端口 → 透明落到 mock 来源，客户端
#      拿到 200 且 model = 请求名，日志记 attempts=2 与最终来源名
#  13) GET /v1/models：只列可服务模型，原始名字典序（停用的模型不出现）
#
# 单端口（2026-08-06 决策：管理面与数据面同监听器）：
#  14) 同一端口上 / → 302 /ui/，出厂默认管理员（gate）能登进管理台并读到
#      admin-only 的读数
#
# 管理 API 未知路径：
#  15) 匿名请求一律先答 401，不给未认证方端点探测面；带会话才答 404，
#      日志里也不应出现默认口令。
#
# Agents / Codex 订阅代理（iteration-11）：
#  16) Key 没有开发工具授权时 POST /agents/codex/v1/responses → 403 devtool_not_allowed（别名 /agents/v1/responses → 403 subscription_not_allowed）
#      （非 5xx，OpenAI 风格错误体）。这一例同时证明**本机冒烟不出网**：
#      没有订阅行就在取账号那一步返回，一个字节都不发往 OpenAI。
#
# 标准 Responses 目录面（2026-08-13，docs/firmware-gateway.md「Responses 目录面」）：
#  17) POST /v1/responses 走模型目录：非流式（response 对象、model = 请求名、
#      output_text 有正文、usage 已映射）+ SSE（response.created 起、
#      response.completed 终、无 [DONE]）。上游是 chat 端点，双向转换在设备上。
#
# 默认管理员改密（本例会**真的改掉 gate 的口令**，所以排在最后）：
#  18) 用默认口令登录 → 本人改密 → 旧口令登不进来、新口令能登进来。
#      这一条钉住的是「出厂口令只是起点」：交付后第一件事就该在管理台改掉它。
#
# 全部通过退出码 0，任一失败非 0（后续迭代回归复用）。
#
# 端口缺省 18090/18091（与 dev 常用的 8080/18080 错开，避免与手工起的实例
# 冲突），可用 SMOKE_GW_PORT / SMOKE_MOCK_PORT 覆盖。

set -u -o pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GW_PORT="${SMOKE_GW_PORT:-18090}"
MOCK_PORT="${SMOKE_MOCK_PORT:-18091}"
DEAD_PORT="${SMOKE_DEAD_PORT:-1}" # 必然拒绝连接：故障切换的高优先级来源
GW="http://127.0.0.1:${GW_PORT}"
KEY="sk_smoke_example_only"
# 出厂默认登录口令（internal/auth 的 DefaultPassword）。设备只有一个管理员、
# 一个口令；它是编译进固件的公开缺省，交付后必须在管理台改掉——第 18 例走的
# 就是那条路。
ADMIN_PASS="llm-gate"
ADMIN_NEW_PASS="smoke-pass-1234"
TMP="$(mktemp -d)"
MOCK_PID=""
GW_PID=""
TOTAL=18

cleanup() {
  [ -n "$GW_PID" ] && kill "$GW_PID" 2>/dev/null
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$TMP"
}
trap cleanup EXIT

dump_logs() {
  echo "--- gatewayd 日志（尾部） ---"
  tail -n 20 "$TMP/gw.log" 2>/dev/null
  echo "--- mock-openai 日志（尾部） ---"
  tail -n 20 "$TMP/mock.log" 2>/dev/null
}

echo "== 构建 =="
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$TMP/llmgate" ./cmd/llmgate) || exit 1
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$TMP/mock-openai" ./tools/mock-openai) || exit 1

# upstreams / logical_models 是 seed 段：只在空库首启导入（$TMP/data 每次都是
# 新目录，故每跑一次冒烟都完整 seed 一遍）。导入后模型名 = upstream_model_id。
#
# 故障切换链：mock-dead（死端口）与 mock-main 挂同一个 mock-gpt-4o-mini；
# 段内优先级按逻辑名字典序推进 100/200，故 a-failover-dead 先于 b-failover-main。
cat > "$TMP/smoke.yaml" <<EOF
# 单端口：管理面与数据面同监听器（2026-08-06 决策），不再有 admin_listen。
listen: "127.0.0.1:${GW_PORT}"
data_dir: "${TMP}/data"
log_level: "debug"
upstreams:
  - name: mock-dead
    type: mock
    base_url: "http://127.0.0.1:${DEAD_PORT}/v1"
    api_key: "mock-key-placeholder"
  - name: mock-main
    type: mock
    base_url: "http://127.0.0.1:${MOCK_PORT}/v1"
    api_key: "mock-key-placeholder"
  # 仅供协议过滤用例：ark（方舟按量）无内置 anthropic 端点，/v1/messages 侧
  # 在选路阶段即 404，不会向真实上游发出任何请求。
  - name: ark-chat-only
    type: ark
    api_key: "ark-key-placeholder-not-real"
logical_models:
  a-failover-dead:
    upstream: mock-dead
    upstream_model_id: mock-gpt-4o-mini
  b-failover-main:
    upstream: mock-main
    upstream_model_id: mock-gpt-4o-mini
  c-claude:
    upstream: mock-main
    upstream_model_id: mock-claude-mini
  d-ark-only:
    upstream: ark-chat-only
    upstream_model_id: smoke-ark-only
api_keys:
  - key: "${KEY}"
# 官网只读入口指到必然拒绝连接的本机死端口：启动时的数据升级检查不会访问
# 生产官网，失败也不得影响网关冒烟。
official_site:
  base_url: "http://127.0.0.1:${DEAD_PORT}"
EOF

echo "== 启动 mock-openai(:${MOCK_PORT}) 与 gatewayd(:${GW_PORT}) =="
"$TMP/mock-openai" --listen "127.0.0.1:${MOCK_PORT}" --sse-interval 20ms >"$TMP/mock.log" 2>&1 &
MOCK_PID=$!
"$TMP/llmgate" gatewayd --config "$TMP/smoke.yaml" >"$TMP/gw.log" 2>&1 &
GW_PID=$!

wait_http() { # $1=url $2=组件名
  local i
  for i in $(seq 1 50); do
    curl -sf -o /dev/null --max-time 1 "$1" && return 0
    sleep 0.1
  done
  echo "FAIL: $2 未就绪（$1）"
  dump_logs
  return 1
}
wait_http "http://127.0.0.1:${MOCK_PORT}/v1/models" "mock-openai" || exit 1
wait_http "${GW}/healthz" "gatewayd" || exit 1

pass=0
fail=0

echo "== 1) chat 非流式透传（model = 请求名） =="
resp="$(curl -s --max-time 15 -w $'\n%{http_code}' \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","messages":[{"role":"user","content":"hello"}],"x_client_extra":"keep-me"}' \
  "${GW}/v1/chat/completions")"
code="${resp##*$'\n'}"
body="${resp%$'\n'*}"
# mock 响应 model 返带版本后缀的上游侧 ID（mock-claude-mini-260801），
# 客户端必须只看到自己请求的名字。
if [ "$code" = "200" ] \
  && echo "$body" | grep -q '"x_mock_extra"' \
  && echo "$body" | grep -q '"x_client_extra"' \
  && echo "$body" | grep -q '"content"' \
  && echo "$body" | grep -q '"model":"mock-claude-mini"' \
  && ! echo "$body" | grep -q '260801'; then
  echo "PASS: 200，未知字段双向透传，model = 请求名"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=${body}"
  fail=1
fi

echo "== 2) chat SSE（未带 stream_options，网关注入 include_usage） =="
sse="$(curl -sN --max-time 20 \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}' \
  "${GW}/v1/chat/completions")"
if echo "$sse" | grep -q '"usage"' && echo "$sse" | grep -q '^data: \[DONE\]' \
  && echo "$sse" | grep -q '"model":"mock-claude-mini"' \
  && ! echo "$sse" | grep -q '260801'; then
  echo "PASS: usage 终块与 [DONE] 到达客户端，chunk model = 请求名"
  pass=$((pass + 1))
else
  echo "FAIL: SSE 输出缺 usage/[DONE] 或 model 不是请求名:"
  echo "$sse"
  fail=1
fi

echo "== 3) chat 无 Key 401 =="
code="$(curl -s -o "$TMP/err3.json" -w '%{http_code}' --max-time 15 \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-gpt-4o-mini","messages":[]}' \
  "${GW}/v1/chat/completions")"
if [ "$code" = "401" ] && grep -q '"missing_api_key"' "$TMP/err3.json"; then
  echo "PASS: 401 missing_api_key"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=$(cat "$TMP/err3.json" 2>/dev/null)"
  fail=1
fi

echo "== 4) chat 未知模型 404（含已失效的旧逻辑名） =="
code="$(curl -s -o "$TMP/err4.json" -w '%{http_code}' --max-time 15 \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"chat-fast","messages":[]}' \
  "${GW}/v1/chat/completions")"
if [ "$code" = "404" ] && grep -q '"model_not_found"' "$TMP/err4.json"; then
  echo "PASS: 404 model_not_found（旧逻辑名已失效）"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=$(cat "$TMP/err4.json" 2>/dev/null)"
  fail=1
fi

echo "== 5) messages 非流式透传（x-api-key 鉴权） =="
resp="$(curl -s --max-time 15 -w $'\n%{http_code}' \
  -H "x-api-key: ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"x_client_extra":"keep-me"}' \
  "${GW}/v1/messages")"
code="${resp##*$'\n'}"
body="${resp%$'\n'*}"
if [ "$code" = "200" ] \
  && echo "$body" | grep -q '"x_mock_extra"' \
  && echo "$body" | grep -q '"x_client_extra"' \
  && echo "$body" | grep -q '"stop_reason"' \
  && echo "$body" | grep -q '"model":"mock-claude-mini"' \
  && ! echo "$body" | grep -q '260801'; then
  echo "PASS: 200，x-api-key 鉴权通过，未知字段双向透传，model = 请求名"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=${body}"
  fail=1
fi

echo "== 6) messages SSE（事件序列完整到 message_stop） =="
sse="$(curl -sN --max-time 20 \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}' \
  "${GW}/v1/messages")"
# 事件名序列必须完整有序：message_start → ping → content_block_start →
# content_block_delta ×N → content_block_stop → message_delta → message_stop；
# message_start 的 message.model = 请求名（未知字段 x_mock_extra 保留）。
events="$(echo "$sse" | sed -n 's/^event: //p' | tr '\n' ' ')"
seq_ok=0
case "$events" in
  "message_start ping content_block_start content_block_delta "*"content_block_stop message_delta message_stop ") seq_ok=1 ;;
esac
if [ "$seq_ok" = "1" ] \
  && echo "$sse" | grep -q '"model":"mock-claude-mini"' \
  && echo "$sse" | grep -q '"x_mock_extra"' \
  && ! echo "$sse" | grep -q '260801'; then
  echo "PASS: 事件序列完整到 message_stop，message_start 的 model = 请求名"
  pass=$((pass + 1))
else
  echo "FAIL: 事件序列不完整或 model 不是请求名（events=${events}）:"
  echo "$sse"
  fail=1
fi

echo "== 7) messages 无 Key 401（Anthropic 风格） =="
code="$(curl -s -o "$TMP/err7.json" -w '%{http_code}' --max-time 15 \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","max_tokens":64,"messages":[]}' \
  "${GW}/v1/messages")"
if [ "$code" = "401" ] && grep -q '"type":"error"' "$TMP/err7.json" \
  && grep -q '"authentication_error"' "$TMP/err7.json"; then
  echo "PASS: 401 Anthropic 风格 authentication_error"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=$(cat "$TMP/err7.json" 2>/dev/null)"
  fail=1
fi

echo "== 8) count_tokens 透传 =="
resp="$(curl -s --max-time 15 -w $'\n%{http_code}' \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","messages":[{"role":"user","content":"count my tokens"}]}' \
  "${GW}/v1/messages/count_tokens")"
code="${resp##*$'\n'}"
body="${resp%$'\n'*}"
body="${body%$'\n'}"
if [ "$code" = "200" ] && echo "$body" | grep -Eq '"input_tokens":[1-9][0-9]*'; then
  echo "PASS: 200 上游计数透传 ${body}"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=${body}"
  fail=1
fi

echo "== 9) count_tokens 上游 404 → 本地粗估兜底 =="
resp="$(curl -s --max-time 15 -w $'\n%{http_code}' \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","messages":[{"role":"user","content":"count my tokens"}],"x_mock_count_404":true}' \
  "${GW}/v1/messages/count_tokens")"
code="${resp##*$'\n'}"
body="${resp%$'\n'*}"
body="${body%$'\n'}"
# mock 收到 x_mock_count_404 返 404；网关本地粗估兜底仍回 200 的
# {"input_tokens":N}，并记 count_tokens_source="fallback_heuristic" 日志。
if [ "$code" = "200" ] && echo "$body" | grep -Eq '"input_tokens":[1-9][0-9]*' \
  && grep -q 'fallback_heuristic' "$TMP/gw.log"; then
  echo "PASS: 上游 404 时本地粗估兜底 ${body}（日志含 fallback_heuristic）"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=${body} gw.log_fallback=$(grep -c 'fallback_heuristic' "$TMP/gw.log" 2>/dev/null)"
  fail=1
fi

echo "== 10) 双入口同名模型（同一个 mock-claude-mini 两边都通） =="
# 用单来源的 mock-claude-mini：故障切换留给 12) 独占 mock-gpt-4o-mini，
# 那条用例的日志断言才不会被本例提前满足。
code_chat="$(curl -s -o "$TMP/dual_chat.json" -w '%{http_code}' --max-time 15 \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","messages":[{"role":"user","content":"hi"}]}' \
  "${GW}/v1/chat/completions")"
code_msg="$(curl -s -o "$TMP/dual_msg.json" -w '%{http_code}' --max-time 15 \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude-mini","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}' \
  "${GW}/v1/messages")"
if [ "$code_chat" = "200" ] && [ "$code_msg" = "200" ] \
  && grep -q '"model":"mock-claude-mini"' "$TMP/dual_chat.json" \
  && grep -q '"model":"mock-claude-mini"' "$TMP/dual_msg.json"; then
  echo "PASS: 同一模型名 chat 与 messages 两入口都 200，model 均为请求名"
  pass=$((pass + 1))
else
  echo "FAIL: chat code=${code_chat} body=$(cat "$TMP/dual_chat.json" 2>/dev/null)"
  echo "      messages code=${code_msg} body=$(cat "$TMP/dual_msg.json" 2>/dev/null)"
  fail=1
fi

echo "== 11) 协议过滤：仅 ark 来源的模型进 /v1/messages → 404 指向 chat 入口 =="
code="$(curl -s -o "$TMP/err11.json" -w '%{http_code}' --max-time 15 \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"smoke-ark-only","max_tokens":64,"messages":[]}' \
  "${GW}/v1/messages")"
if [ "$code" = "404" ] && grep -q '"not_found_error"' "$TMP/err11.json" \
  && grep -q 'chat/completions' "$TMP/err11.json" \
  && ! grep -q 'ark-chat-only' "$TMP/err11.json"; then
  echo "PASS: 404 指向 chat 入口，且不泄露上游账户名"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=$(cat "$TMP/err11.json" 2>/dev/null)"
  fail=1
fi

echo "== 12) 优先级故障切换（高优先级来源指死端口 → 透明落 mock） =="
# mock-gpt-4o-mini 是全配置里唯一挂两条来源的模型，且只有本例请求它：
# 下面的日志断言（attempts=2 / 最终来源 / 切换记录）只可能由本次请求满足。
before_switch="$(grep -c '切换下一来源' "$TMP/gw.log" 2>/dev/null)"
resp="$(curl -s --max-time 15 -w $'\n%{http_code}' \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-gpt-4o-mini","messages":[{"role":"user","content":"failover"}]}' \
  "${GW}/v1/chat/completions")"
code="${resp##*$'\n'}"
body="${resp%$'\n'*}"
# 访问日志应记 attempts=2 与最终来源名 mock-main（切换对客户端无感）。
after_switch="$(grep -c '切换下一来源' "$TMP/gw.log" 2>/dev/null)"
if [ "$code" = "200" ] \
  && echo "$body" | grep -q '"model":"mock-gpt-4o-mini"' \
  && ! echo "$body" | grep -q '260801' \
  && [ "$after_switch" -gt "$before_switch" ] \
  && grep -q '"attempts":2' "$TMP/gw.log" \
  && grep -q '"upstream":"mock-main"' "$TMP/gw.log" \
  && grep -q '上游连接失败，切换下一来源' "$TMP/gw.log"; then
  echo "PASS: 首选来源连不上时透明切换，客户端拿到 200 且 model = 请求名"
  pass=$((pass + 1))
else
  echo "FAIL: code=${code} body=${body}"
  echo "      switch_before=${before_switch} switch_after=${after_switch} attempts=$(grep -c '"attempts":2' "$TMP/gw.log" 2>/dev/null)"
  fail=1
fi

echo "== 13) GET /v1/models 只列可服务模型（原始名字典序） =="
models="$(curl -s --max-time 15 -H "Authorization: Bearer ${KEY}" "${GW}/v1/models")"
ids="$(echo "$models" | tr ',' '\n' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | tr '\n' ' ')"
if [ "$ids" = "mock-claude-mini mock-gpt-4o-mini smoke-ark-only " ] \
  && ! echo "$models" | grep -q 'mock-main' \
  && ! echo "$models" | grep -q 'mock-dead'; then
  echo "PASS: 字典序原始名列表（${ids}），不泄露上游账户名"
  pass=$((pass + 1))
else
  echo "FAIL: ids=[${ids}] body=${models}"
  fail=1
fi

echo "== 14) 管理面与数据面同端口（/ → 302 /ui/，出厂默认口令能登进来） =="
redirect="$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' --max-time 15 "${GW}/")"
login_code="$(curl -s --max-time 15 -o "$TMP/login14.json" -w '%{http_code}' \
  -c "$TMP/cookie14.txt" \
  -H "X-LlmGate-CSRF: 1" -H "Content-Type: application/json" \
  -d "{\"password\":\"${ADMIN_PASS}\"}" \
  "${GW}/admin/v1/login")"
# 会话真能用：探针 204 + 随手读一条业务端点（空库也回 200）。
probe_code="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' \
  -b "$TMP/cookie14.txt" "${GW}/admin/v1/session")"
keys_code="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' \
  -b "$TMP/cookie14.txt" "${GW}/admin/v1/keys")"
if [ "$redirect" = "302 ${GW}/ui/" ] && [ "$login_code" = "204" ] \
  && [ "$probe_code" = "204" ] && [ "$keys_code" = "200" ]; then
  echo "PASS: 同端口 / → 302 /ui/，默认口令登录 204 且会话可用"
  pass=$((pass + 1))
else
  echo "FAIL: redirect=[${redirect}] login=${login_code} session=${probe_code} keys=${keys_code}"
  echo "      body=$(cat "$TMP/login14.json" 2>/dev/null)"
  fail=1
fi

echo "== 15) 管理 API 未知路径匿名 401，且口令不进日志 =="
# 匿名拿到的是 401 而不是 404：/admin/v1/ 下的未知路径先过会话中间件再落 404，
# 不给未认证方一张"这台设备有哪些端点"的探测面。带会话时才是统一 JSON 404。
unknown_path="/admin/v1/not-a-real-endpoint"
get_unknown="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' "${GW}${unknown_path}")"
post_unknown="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' \
  -H "X-LlmGate-CSRF: 1" -H "Content-Type: application/json" -d '{}' "${GW}${unknown_path}")"
with_session="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' \
  -b "$TMP/cookie14.txt" "${GW}${unknown_path}")"
# 出厂口令不该出现在任何级别的日志里（本次冒烟跑的是 debug）。
pass_leak=""
grep -q "\"${ADMIN_PASS}\"" "$TMP/gw.log" && pass_leak="默认口令"
if [ "$get_unknown" = "401" ] && [ "$post_unknown" = "401" ] && [ "$with_session" = "404" ] \
  && [ -z "$pass_leak" ]; then
  echo "PASS: 未知端点匿名 401、带会话 404，日志无口令"
  pass=$((pass + 1))
else
  echo "FAIL: 状态不符：GET=${get_unknown} POST=${post_unknown} with_session=${with_session}"
  [ -n "$pass_leak" ] && echo "  （日志里出现了${pass_leak}——§15.1 事故）"
  fail=1
fi

echo "== 16) 接入面：Key 未获 Codex 授权时 /agents/codex/v1/responses 回 403 devtool_not_allowed，别名 /agents/v1/responses 回 403 subscription_not_allowed =="
# 「没连订阅」是状态不是故障，所以是 4xx 而不是 5xx。正式接入面先过子树闸
# （这把 Key 既没勾 Codex 订阅也没有目录模型 → devtool_not_allowed），兼容别名
# 按 model 前缀分流后过订阅闸（subscription_not_allowed）。鉴权与准入都要真过
# 一遍，所以带的是有效的 smoke Key。
code="$(curl -s -o "$TMP/err16.json" -w '%{http_code}' --max-time 15 \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-codex","input":"hi"}' \
  "${GW}/agents/codex/v1/responses")"
code16b="$(curl -s -o "$TMP/err16b.json" -w '%{http_code}' --max-time 15 \
  -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-codex","input":"hi"}' \
  "${GW}/agents/v1/responses")"
if [ "$code" = "403" ] && grep -q '"devtool_not_allowed"' "$TMP/err16.json" &&
   [ "$code16b" = "403" ] && grep -q '"subscription_not_allowed"' "$TMP/err16b.json"; then
  echo "PASS: 接入面 403 devtool_not_allowed，别名 403 subscription_not_allowed（失败闭合，且未出网）"
  pass=$((pass + 1))
else
  echo "FAIL: face=${code} $(cat "$TMP/err16.json" 2>/dev/null) alias=${code16b} $(cat "$TMP/err16b.json" 2>/dev/null)"
  fail=1
fi

echo "== 17) Responses 目录面：/v1/responses 非流式 + SSE 双向转换 =="
# 模型走目录选路（mock-gpt-4o-mini 的 openai 侧来源），设备把 Responses 请求
# 转成 chat 打上游、把 chat 响应合成回 Responses 形。非流式看 response 对象
# 三要素（object/model 回写/正文），SSE 看事件序首尾与「无 [DONE]」。
resp17="$(curl -s --max-time 15 -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-gpt-4o-mini","store":false,"instructions":"Be brief.","input":"hello"}' \
  "${GW}/v1/responses")"
sse17="$(curl -sN --max-time 15 -H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json" \
  -d '{"model":"mock-gpt-4o-mini","store":false,"stream":true,"input":"hi"}' \
  "${GW}/v1/responses")"
if echo "$resp17" | grep -q '"object":"response"' \
  && echo "$resp17" | grep -q '"model":"mock-gpt-4o-mini"' \
  && echo "$resp17" | grep -q '"output_text"' \
  && echo "$resp17" | grep -q '"input_tokens"' \
  && echo "$sse17" | grep -q 'event: response.created' \
  && echo "$sse17" | grep -q 'event: response.completed' \
  && ! echo "$sse17" | grep -q '\[DONE\]'; then
  echo "PASS: 非流式 response 对象 + SSE 事件序（created→completed，无 [DONE]）"
  pass=$((pass + 1))
else
  echo "FAIL: resp17=${resp17} sse17_head=$(echo "$sse17" | head -4)"
  fail=1
fi

echo "== 18) 出厂口令只是起点：改密后旧口令即失效 =="
# 本例真的会改掉设备口令（$TMP/data 每跑一次都是新目录），所以排在最后。
# 改密要验旧口令，清空全部旧会话，并为当前浏览器换发新会话。
chg="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' \
  -b "$TMP/cookie14.txt" \
  -H "X-LlmGate-CSRF: 1" -H "Content-Type: application/json" \
  -d "{\"old_password\":\"${ADMIN_PASS}\",\"new_password\":\"${ADMIN_NEW_PASS}\"}" \
  "${GW}/admin/v1/password")"
old_login="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' \
  -H "X-LlmGate-CSRF: 1" -H "Content-Type: application/json" \
  -d "{\"password\":\"${ADMIN_PASS}\"}" \
  "${GW}/admin/v1/login")"
new_login="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' \
  -H "X-LlmGate-CSRF: 1" -H "Content-Type: application/json" \
  -d "{\"password\":\"${ADMIN_NEW_PASS}\"}" \
  "${GW}/admin/v1/login")"
# §15.1：新旧口令在任何日志级别都不落盘。
pw_leak=""
grep -q "${ADMIN_NEW_PASS}" "$TMP/gw.log" && pw_leak="新口令"
grep -q "\"${ADMIN_PASS}\"" "$TMP/gw.log" && pw_leak="${pw_leak} 旧口令"
if [ "$chg" = "204" ] && [ "$old_login" = "401" ] && [ "$new_login" = "204" ] \
  && [ -z "$pw_leak" ]; then
  echo "PASS: 改密 204，旧口令 401、新口令 204，日志无口令"
  pass=$((pass + 1))
else
  echo "FAIL: change=${chg} old_login=${old_login} new_login=${new_login}"
  [ -n "$pw_leak" ] && echo "  （日志里出现了${pw_leak}——§15.1 事故）"
  fail=1
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "SMOKE PASS（${pass}/${TOTAL}）"
else
  echo "SMOKE FAIL（${pass}/${TOTAL} 通过）"
  dump_logs
fi
exit "$fail"
