package admin

import (
	"context"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// CursorModelPrices supplies the usage report with the same read-only catalog
// used by the subscription page and gateway billing.
func (s *Server) CursorModelPrices(ctx context.Context) map[string]usage.Pricing {
	doc, _ := s.effectivePlatformModels(ctx)
	agent, _ := doc.Agent(store.AgentProviderCursor)
	prices := make(map[string]usage.Pricing, len(agent.Models))
	for _, model := range agent.Models {
		if price, err := usage.ParseCursorPrice(string(model.Pricing)); err == nil {
			prices[model.Name] = price
		}
	}
	return prices
}

type cursorPriceModel struct {
	Name    string        `json:"name"`
	Pricing usage.Pricing `json:"pricing"`
}

// Cursor 计价清单只读生效平台目录；不从调用历史、本地模型或覆盖设置补条目。
func (s *Server) handleCursorPrices(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.st.ListAgentAccounts(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	models := make([]cursorPriceModel, 0)
	for _, account := range accounts {
		if account.Provider != store.AgentProviderCursor {
			continue
		}
		doc, _ := s.effectivePlatformModels(r.Context())
		agent, _ := doc.Agent(store.AgentProviderCursor)
		for _, model := range agent.Models {
			price, _ := usage.ParseCursorPrice(string(model.Pricing))
			models = append(models, cursorPriceModel{Name: model.Name, Pricing: price})
		}
		break
	}
	writeJSON(w, http.StatusOK, struct {
		Models []cursorPriceModel `json:"models"`
	}{models})
}
