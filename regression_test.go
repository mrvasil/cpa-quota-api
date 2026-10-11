package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPassiveRetryAfterIsAnchoredToOriginalObservation(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	f := &fakeBackend{owns: true, files: []cpaFile{{AuthIndex: "one", Provider: "claude", Quota: signalSet{ObservedAt: at, Signals: map[string]string{"Retry-After": "3600"}}}}}
	s := testService(t, f)
	s.refreshOne(context.Background())
	if f.calls.Load() != 0 {
		t.Fatal("passive retry guidance ignored")
	}
	for _, entry := range s.accounts {
		if !entry.RetryNotBefore.Equal(at.Add(time.Hour)) {
			t.Fatal("retry timestamp not anchored to observation")
		}
	}
	s.readSnapshot(context.Background())
	for _, entry := range s.accounts {
		if !entry.RetryNotBefore.Equal(at.Add(time.Hour)) {
			t.Fatal("reread extended retry deadline")
		}
	}
}
func TestMalformedAccountDoesNotHideValidNeighbor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"files":[{"auth_index":"broken","provider":"claude","quota":{"observed_at":"invalid"}},{"auth_index":"valid","provider":"codex","status":"active"}]}`))
	}))
	defer server.Close()
	b := newBackend(Config{CPAURL: server.URL})
	files, err := b.snapshot(context.Background())
	if err != nil || len(files) != 2 || !files[0].Invalid || files[1].Invalid {
		t.Fatalf("per-account isolation failed: %+v %v", files, err)
	}
}
func TestClaudeScopedIdentityMatchesAcrossSources(t *testing.T) {
	at := time.Now()
	passive := normalizeSignals("claude", signalSet{ObservedAt: at, Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d_sonnet-Utilization": "0.4"}}, nil)
	active, err := normalizeActive("claude", []byte(`{"seven_day_sonnet":{"utilization":50}}`), at.Add(time.Minute), nil)
	merged := mergeWindows(passive, active.Windows)
	if err != nil || len(merged) != 1 || *merged[0].UsedPercent != 50 {
		t.Fatal("same scoped limit split into two windows")
	}
}
