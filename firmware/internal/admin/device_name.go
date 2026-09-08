package admin

import (
	"errors"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func (s *Server) handleSetDeviceName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name, err := s.st.SetDeviceName(r.Context(), req.Name)
	if errors.Is(err, store.ErrInvalidDeviceName) {
		writeError(w, http.StatusBadRequest, "invalid_device_name", err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: "device.name.update", Entity: "system:device", RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, struct {
		Name string `json:"name"`
	}{Name: name})
}
