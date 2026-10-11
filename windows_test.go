package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeStructuredWindowsExcludeBreakdownAndLegacyAliases(t *testing.T) {
	at := time.Now().UTC()
	payload := []byte(`{
		"five_hour":{"utilization":1},
		"seven_day":{"utilization":63},
		"seven_day_sonnet":{"utilization":15},
		"seven_day_breakdown":{"as_of":"2026-10-04T13:00:00Z","rows":[{"percent":100}]},
		"limits":[
			{"kind":"session","percent":1,"is_active":false},
			{"kind":"weekly_all","percent":63},
			{"kind":"weekly_scoped","percent":15,"scope":{"model":{"display_name":"Fable"}}}
		]
	}`)
	a, err := normalizeActive("claude", payload, at, nil)
	if err != nil || len(a.Windows) != 3 {
		t.Fatalf("expected three actual limits, got %+v, %v", a.Windows, err)
	}
	for i, remaining := range []float64{99, 37, 85} {
		if a.Windows[i].RemainingPercent == nil || *a.Windows[i].RemainingPercent != remaining {
			t.Fatalf("incorrect percent for window %d: %+v", i, a.Windows[i])
		}
	}
	if a.Windows[2].Label != "7-day Fable" || a.Windows[2].Scope != "" {
		t.Fatal("scoped limit name lost or execution model exposed")
	}
}

func TestExecutionModelDoesNotDefineAQuotaScope(t *testing.T) {
	at := time.Now()
	set := signalSet{ObservedAt: at, Signals: map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.01",
		"anthropic-ratelimit-unified-7d-utilization": "0.63",
	}}
	f := cpaFile{Provider: "claude", Quota: set, ModelQuotas: map[string]signalSet{"claude-fable-5": set}}
	a := normalizeFile(f, "account", at, nil)
	if len(a.Windows) != 2 {
		t.Fatal("execution model created invented quota")
	}
	for _, w := range a.Windows {
		if w.Group != "main" {
			t.Fatal("account limit mislabelled as model quota")
		}
	}
}

func TestLegacyBreakdownIsNotAQuota(t *testing.T) {
	a, err := normalizeActive("claude", []byte(`{"five_hour":{"utilization":1},"seven_day_breakdown":{"rows":[{"percent":100}]}}`), time.Now(), nil)
	if err != nil || len(a.Windows) != 1 {
		t.Fatal("breakdown became a quota window")
	}
}

func TestCodexAccountSignalsDoNotMultiplyByModelsOrActiveLimit(t *testing.T) {
	at := time.Now()
	set := signalSet{ObservedAt: at, Signals: map[string]string{
		"x-codex-active-limit":             "premium",
		"x-codex-primary-used-percent":     "32",
		"x-codex-primary-window-minutes":   "10080",
		"x-codex-secondary-window-minutes": "0",
	}}
	f := cpaFile{Provider: "codex", Quota: set, ModelQuotas: map[string]signalSet{"model-a": set, "model-b": set}}
	a := normalizeFile(f, "account", at, nil)
	active, err := normalizeActive("codex", []byte(`{"rate_limit":{"primary_window":{"used_percent":29,"limit_window_seconds":604800}}}`), at.Add(-time.Minute), nil)
	merged := mergeWindows(active.Windows, a.Windows)
	if err != nil || len(merged) != 1 || merged[0].Group != "main" || *merged[0].RemainingPercent != 68 {
		t.Fatalf("duplicate or wrong latest value: %+v", merged)
	}
}

func TestPublicViewAndRestartRemovePersistedDuplicates(t *testing.T) {
	at := time.Now().UTC()
	cfg := Config{StateDir: t.TempDir(), SnapshotEvery: time.Minute}
	main := newWindow("primary", "main", ptr(int64(604800)), ptr(32.0), nil, at, "cpa_passive")
	scoped := main
	scoped.Scope = "model_legacy"
	scoped.UsedPercent = ptr(100.0)
	alias := main
	alias.Group = "limit_legacy"
	empty := newWindow("seven_day", "claude-scope_breakdown", ptr(int64(604800)), nil, nil, at, "active_check")
	retry := at.Add(time.Hour)
	entries := map[string]storedAccount{
		"codex":  {Account: account{ID: "codex", Provider: "codex", Windows: []window{main, scoped, alias}}, Failures: 3, NextAttempt: retry, RetryNotBefore: retry},
		"claude": {Account: account{ID: "claude", Provider: "claude", Windows: []window{empty}}},
	}
	data, err := json.Marshal(persisted{Version: 1, Accounts: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "state.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	view := s.view(at)
	if len(view.Providers[0].Accounts[0].Windows) != 1 || len(view.Providers[1].Accounts[0].Windows) != 0 {
		t.Fatal("old persisted duplicates leaked into response")
	}
	e := s.accounts["codex"]
	if e.Failures != 3 || !e.NextAttempt.Equal(retry) || !e.RetryNotBefore.Equal(retry) {
		t.Fatal("migration reset refresh backoff")
	}
}

func TestActiveRefreshReplacesWindowInventoryButPreservesNewerMatchingObservation(t *testing.T) {
	at := time.Now().UTC()
	latest := newWindow("five_hour", "main", ptr(int64(18000)), ptr(20.0), nil, at, "cpa_passive")
	obsolete := newWindow("seven_day", "claude-scope_old", ptr(int64(604800)), ptr(100.0), nil, at.Add(-time.Hour), "active_check")
	incoming := latest
	incoming.ObservedAt = ptr(at.Add(-time.Minute))
	incoming.UsedPercent = ptr(10.0)
	result := reconcileActiveWindows("claude", []window{latest, obsolete}, []window{incoming})
	if len(result) != 1 || *result[0].UsedPercent != 20 {
		t.Fatalf("incorrect replacement: %+v", result)
	}
}
