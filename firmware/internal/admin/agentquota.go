package admin

import (
	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"net/http"
)

func (s *Server) SetAgentQuota(m *agentquota.Manager) { s.agentQuota = m }
func (s *Server) handleSyncAgentQuota(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Agents 账号不存在")
		return
	}
	a, err := s.st.GetAgentAccount(r.Context(), id)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	if s.agentQuota == nil {
		writeError(w, http.StatusServiceUnavailable, "quota_unavailable", "订阅额度同步尚未就绪")
		return
	}
	q := s.agentQuota.Sync(r.Context(), a, true)
	writeJSON(w, http.StatusOK, struct {
		Quota agentquota.Snapshot `json:"quota"`
	}{q})
}
