package store

import (
	"encoding/json"
	"testing"
)

func TestMutateAgentAuthRetriesOnConcurrentWrite(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.UpsertAgentAccount(t.Context(), NewAgentAccount{Provider: AgentProviderClaude, AuthJSON: `{"setup_token":"fake-old-setup","quota":"fake-old-quota"}`})
	if err != nil {
		t.Fatal(err)
	}
	var merges int
	_, err = s.MutateAgentAuth(t.Context(), a.ID, AgentProviderClaude, true, func(a *AgentAccount, blob string) (string, error) {
		var fields map[string]string
		if err := json.Unmarshal([]byte(blob), &fields); err != nil {
			return "", err
		}
		merges++
		if merges == 1 {
			if err := s.SetAgentAuthJSON(t.Context(), a.ID, AgentProviderClaude, `{"setup_token":"fake-new-setup","quota":"fake-old-quota"}`); err != nil {
				return "", err
			}
			if err := s.SetAgentStatus(t.Context(), a.ID, AgentStatusDisabled); err != nil {
				return "", err
			}
		}
		fields["quota"] = "fake-new-quota"
		raw, err := json.Marshal(fields)
		return string(raw), err
	})
	if err != nil {
		t.Fatal(err)
	}
	current, blob, err := s.GetAgentCredential(t.Context(), AgentProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(blob), &fields); err != nil {
		t.Fatal(err)
	}
	if merges != 2 || fields["setup_token"] != "fake-new-setup" || fields["quota"] != "fake-new-quota" || current.Status != AgentStatusDisabled || current.LastRefreshAt.IsZero() {
		t.Fatal("concurrent credential or status update was lost")
	}
}
