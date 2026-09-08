package store

import "testing"

func TestResponsesMigrationPreservesAccess(t *testing.T) {
	dir := t.TempDir()
	db := openLegacyDB(t, dir, 29)
	for _, chat := range []int{0, 1} {
		if _, err := db.Exec(`INSERT INTO models (name, entry_openai, entry_anthropic, created_at, updated_at)
			VALUES (?, ?, 1, ?, ?)`, []string{"chat-off", "chat-on"}[chat], chat, legacyTS, legacyTS); err != nil {
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
	for _, name := range []string{"chat-off", "chat-on"} {
		m, err := st.GetModelByName(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if m.EntryResponses != m.EntryOpenAI || m.EntryOpenAI != (name == "chat-on") || !m.EntryAnthropic {
			t.Fatalf("migration changed access: %+v", m)
		}
	}
}

func TestResponsesOnlyModelReadPaths(t *testing.T) {
	st, dir := mustOpen(t)
	up := mustUpstream(t, st, "chat", "openai_compat", "sk-fake", "https://example.invalid/v1")
	m := mustModel(t, st, "responses-only")
	src := mustSource(t, st, m.ID, up.ID, "", 100)
	if err := st.SetModelEntries(t.Context(), m.ID, false, true, false); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	check := func(m Model) {
		t.Helper()
		if m.EntryOpenAI || !m.EntryResponses || m.EntryAnthropic {
			t.Fatalf("responses-only settings lost: %+v", m)
		}
	}
	models, err := st.ListServableModels(t.Context())
	if err != nil || len(models) != 1 {
		t.Fatalf("servable models: %v, %v", models, err)
	}
	check(models[0])
	rows, err := st.ListServableModelSources(t.Context())
	if err != nil || len(rows) != 1 || rows[0].EntryOpenAI || !rows[0].EntryResponses || rows[0].EntryAnthropic {
		t.Fatalf("servable sources: %v, %v", rows, err)
	}
	route, err := st.ResolveModelRoute(t.Context(), m.Name)
	if err != nil {
		t.Fatal(err)
	}
	check(route.Model)
	sourceRoute, err := st.GetSourceRoute(t.Context(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	check(sourceRoute.Model)
	listed, err := st.ListModelsWithSources(t.Context())
	if err != nil || len(listed) != 1 {
		t.Fatalf("models: %v, %v", listed, err)
	}
	check(listed[0].Model)
}
