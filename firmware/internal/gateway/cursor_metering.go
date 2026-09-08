package gateway

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/cursorwire"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const cursorBidiAppendRPC = "aiserver.v1.BidiService/BidiAppend"
const cursorPendingLimit = 2048
const cursorPendingTTL = 6 * time.Hour

type cursorRequestKey struct {
	keyID  int64
	digest [32]byte
}
type cursorRequestModel struct {
	model    string
	conflict bool
	at       time.Time
}
type cursorRequestModels struct {
	mu      sync.Mutex
	entries map[cursorRequestKey]cursorRequestModel
}

func (c *cursorRequestModels) put(keyID int64, id, model string) {
	if id == "" || !cursorwire.ValidModel(model) {
		return
	}
	key := cursorRequestKey{keyID, sha256.Sum256([]byte(id))}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[cursorRequestKey]cursorRequestModel)
	}
	now := time.Now()
	if existing, ok := c.entries[key]; ok && now.Sub(existing.at) < cursorPendingTTL {
		if existing.model != model {
			existing.conflict = true
			c.entries[key] = existing
		}
		return
	}
	for k, value := range c.entries {
		if now.Sub(value.at) >= cursorPendingTTL {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= cursorPendingLimit {
		return
	}
	c.entries[key] = cursorRequestModel{model: model, at: now}
}

func (c *cursorRequestModels) take(keyID int64, digest [32]byte) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cursorRequestKey{keyID, digest}
	value, ok := c.entries[key]
	delete(c.entries, key)
	if !ok || value.conflict || time.Since(value.at) >= cursorPendingTTL {
		return ""
	}
	return value.model
}

// Call state is shared by the transport's request-reader goroutine and the
// response handler. It holds no body, credentials or subscription handles.
type cursorCallUsage struct {
	mu                             sync.Mutex
	model                          string
	id                             [32]byte
	hasID, conflict, ended, failed bool
	end                            cursorwire.End
}

func (c *cursorCallUsage) request(data []byte, isJSON, bidi bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bidi {
		id := cursorwire.RequestID(data, isJSON)
		if id != "" {
			c.id = sha256.Sum256([]byte(id))
			c.hasID = true
		}
	} else if model := cursorwire.RunModel(data, isJSON); model != "" {
		if c.model != "" && c.model != model {
			c.conflict = true
		} else {
			c.model = model
		}
	}
}

func (c *cursorCallUsage) fail() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed = true
}

func (c *cursorCallUsage) response(data []byte, isJSON bool) {
	end, found := cursorwire.TurnEnd(data, isJSON)
	if !found {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		if c.end != end {
			c.end.Complete = false
		}
		return
	}
	c.end, c.ended = end, true
}

func (s *Server) observeCursorCall(r *http.Request, rpc string) {
	call := new(cursorCallUsage)
	infoFrom(r.Context()).bill.cursor = call
	observer := cursorwire.NewObserver(r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), func(data []byte, isJSON bool) {
		call.request(data, isJSON, rpc == usage.CursorAgentRunSSERPC)
	})
	if encoding := r.Header.Get("Connect-Content-Encoding"); encoding != "" {
		observer = cursorwire.NewObserver(r.Header.Get("Content-Type"), encoding, func(data []byte, isJSON bool) { call.request(data, isJSON, rpc == usage.CursorAgentRunSSERPC) })
	}
	r.Body = cursorwire.ObserveReader(r.Body, observer)
}

func (s *Server) observeCursorAppend(r *http.Request, rpc string, body []byte) {
	if rpc != cursorBidiAppendRPC {
		return
	}
	observer := cursorwire.NewObserver(r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), func(data []byte, isJSON bool) {
		id, model := cursorwire.AppendModel(data, isJSON)
		s.cursorRequests.put(infoFrom(r.Context()).keyID, id, model)
	})
	observer.Write(body)
	observer.Close()
}

func (s *Server) finishCursorCall(r *http.Request) {
	info := infoFrom(r.Context())
	call := info.bill.cursor
	if call == nil {
		return
	}
	call.mu.Lock()
	model, digest, hasID, conflict, end, ended := call.model, call.id, call.hasID, call.conflict, call.end, call.ended
	call.mu.Unlock()
	if hasID {
		model = s.cursorRequests.take(info.keyID, digest)
	}
	if conflict {
		model = ""
	}
	// Auto is a local accounting dimension. Keep the upstream wire bytes and
	// CLI model selection intact; display names do not identify the routed model.
	if model == "default" || model == "auto" {
		model = "cursor-auto"
	}
	if cursorwire.ValidModel(model) {
		info.bill.model = model
	}
	if cursorwire.ValidModel(model) && s.store != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
		defer cancel()
		// Cursor 的同名模型价格独立于 API 与其他订阅，只认目录中的精确 ID。
		agent, _ := s.effectivePlatformModels(ctx).Agent(store.AgentProviderCursor)
		for _, entry := range agent.Models {
			if entry.Name == model && entry.Kind == store.ModelKindText {
				info.bill.modelKnown = true
				if _, err := usage.ParseCursorPrice(string(entry.Pricing)); err == nil {
					info.bill.pricing = string(entry.Pricing)
				}
				break
			}
		}
	}
	if ended {
		info.bill.tokens = usage.Tokens{Prompt: end.Input, Completion: end.Output, CacheRead: end.CacheRead, CacheWrite: end.CacheWrite}
		info.bill.usageSeen = end.Complete
	}
}
