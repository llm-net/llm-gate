package store

import "testing"

func TestMigration0059PreservesModelsAndReferences(t *testing.T) {
	dir := t.TempDir()
	sealer := legacySealer(t, dir)
	sealed, err := sealer.sealKey("fake-systemone-migration-key")
	if err != nil {
		t.Fatal(err)
	}
	db := openLegacyDB(t, dir, 58)
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO upstreams (id,name,type,api_key_sealed,base_url,egress_mode,created_at,updated_at) VALUES (7,'source','openai_compat',?,'https://example.invalid/v1','direct',?,?)`, []any{sealed, legacyTS, legacyTS}},
		{`INSERT INTO models (id,name,kind,family,pricing,entry_openai,entry_responses,entry_anthropic,disabled,created_at,updated_at) VALUES (8,'kept','text','','{"in":123,"out":456}',0,1,0,1,?,?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO model_sources (id,model_id,upstream_id,upstream_model_id,priority,created_at,updated_at) VALUES (9,8,7,'mapped',17,?,?)`, []any{legacyTS, legacyTS}},
		{`INSERT INTO api_keys (id,key_digest,created_at) VALUES (4,'fake-digest',?)`, []any{legacyTS}},
		{`INSERT INTO api_key_api_model_configs (key_id,restricted,revision,updated_at) VALUES (4,1,1,?)`, []any{legacyTS}},
		{`INSERT INTO api_key_api_models (key_id,model_id) VALUES (4,8)`, nil},
		{`INSERT INTO api_key_devtool_models (key_id,model_id) VALUES (4,8)`, nil},
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	route, err := st.ResolveModelRoute(t.Context(), "kept")
	if err != nil {
		t.Fatal(err)
	}
	m := route.Model
	if m.ID != 8 || m.EntryOpenAI || !m.EntryResponses || m.EntryAnthropic || !m.Disabled || m.Pricing != `{"in":123,"out":456}` || len(route.Candidates) != 1 {
		t.Fatalf("model lost: %+v", m)
	}
	c := route.Candidates[0]
	if c.SourceID != 9 || c.Upstream.ID != 7 || c.Upstream.APIKey != "fake-systemone-migration-key" || c.Upstream.EgressMode != "direct" || c.UpstreamModelID != "mapped" {
		t.Fatal("source or credential changed")
	}
	scope, err := st.LookupKeyAPIModelScope(t.Context(), 4)
	if err != nil || !scope.Allows(8) || scope.Allows(9) {
		t.Fatal("authorization changed", err)
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM api_key_devtool_models WHERE key_id=4 AND model_id=8`).Scan(&count); err != nil || count != 1 {
		t.Fatal("dev tool reference lost", err)
	}
	if _, err := st.CreateUpstream(t.Context(), "semif", "systemone", "fake-semif-key", "http://127.0.0.1:18080"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateModel(t.Context(), "judge", ModelKindSystemOne, ""); err != nil {
		t.Fatal(err)
	}
}
