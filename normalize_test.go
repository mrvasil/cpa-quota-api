package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPassiveCodexDurationGroupsAndPrivacy(t *testing.T) {
	now := time.Now().UTC()
	salt := []byte(strings.Repeat("s", 32))
	f := cpaFile{AuthIndex: "private@example.test", Provider: "codex", Status: "active", Quota: signalSet{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "0", "X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-After-Seconds": "123",
		"X-Codex-Secondary-Used-Percent": "0", "X-Codex-Secondary-Window-Minutes": "0",
		"X-Codex-Additional-Secret-Primary-Used-Percent": "33", "X-Codex-Additional-Secret-Primary-Window-Minutes": "300", "X-Codex-Additional-Secret-Limit-Name": "private@example.test",
		"X-Codex-Plan-Type": "pro", "X-Codex-Credits-Balance": "62500",
	}}}
	a := normalizeFile(f, opaqueID(salt, f.Provider, f.AuthIndex), now, salt)
	if len(a.Windows) != 2 || a.Plan != "pro" || a.Routing.Available == nil || !*a.Routing.Available {
		t.Fatalf("unexpected normalized account: %+v", a)
	}
	found := false
	for _, w := range a.Windows {
		if w.Group == "main" {
			found = true
			if w.Label != "weekly" || *w.DurationSeconds != 604800 || *w.UsedPercent != 0 || *w.RemainingPercent != 100 || !w.ResetAt.Equal(now.Add(123*time.Second)) {
				t.Fatalf("window semantics lost: %+v", w)
			}
		}
	}
	if !found {
		t.Fatal("missing main window")
	}
	encoded, _ := json.Marshal(a)
	for _, secret := range []string{"private@example.test", "62500", "Secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private data exposed: %s", secret)
		}
	}
	if opaqueID(salt, "codex", f.AuthIndex) == opaqueID(salt, "claude", f.AuthIndex) {
		t.Fatal("provider IDs collided")
	}
}
func TestClaudeUnitsAreSourceSpecific(t *testing.T) {
	at := time.Now().UTC()
	passive := normalizeSignals("claude", signalSet{ObservedAt: at, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.01", "Anthropic-Ratelimit-Unified-5h-Reset": "1791424800"}}, nil)
	active, err := normalizeActive("claude", []byte(`{"five_hour":{"utilization":0.01,"resets_at":"2030-01-01T00:00:00Z"},"seven_day_sonnet":{"utilization":25},"extra_usage":{"is_enabled":false}}`), at, nil)
	if err != nil || len(active.Windows) != 2 || *passive[0].UsedPercent != 1 || *active.Windows[0].UsedPercent != 0.01 || active.AdditionalUsage != "disabled" {
		t.Fatalf("fraction/percentage confusion: %+v %+v", passive, active)
	}
}
func TestUnknownAndInvalidNumbersNeverBecomeZero(t *testing.T) {
	at := time.Now().UTC()
	for _, value := range []string{"", "NaN", "Inf", "-1", "101", "1 trailing"} {
		windows := normalizeSignals("codex", signalSet{ObservedAt: at, Signals: map[string]string{"X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Used-Percent": value}}, nil)
		if len(windows) != 1 || windows[0].UsedPercent != nil || windows[0].RemainingPercent != nil {
			t.Fatalf("invalid value accepted: %q", value)
		}
	}
	windows := normalizeSignals("claude", signalSet{ObservedAt: at, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "1.5"}}, nil)
	if windows[0].UsedPercent != nil {
		t.Fatal("fraction interpreted as percentage")
	}
}
func TestActiveCodexAllWindowsAndReset(t *testing.T) {
	at := time.Now().UTC()
	a, err := normalizeActive("codex", []byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":7,"limit_window_seconds":604800,"reset_after_seconds":600},"secondary_window":{"used_percent":0,"limit_window_seconds":0}},"code_review_rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}},"additional_rate_limits":[{"limit_name":"GPT-example","rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":42,"reset_at":1900000000}}}]}`), at, nil)
	if err != nil || len(a.Windows) != 3 || a.Plan != "pro" {
		t.Fatalf("missing groups: %+v %v", a, err)
	}
	if a.Windows[0].Label != "weekly" || !a.Windows[0].ResetAt.Equal(at.Add(600*time.Second)) || a.Windows[2].Label != "custom" || a.Windows[2].State != "exhausted" {
		t.Fatal("incorrect window interpretation")
	}
}
func TestFreshnessNeverAdvancesAndResetIsNotRecovery(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	w := newWindow("primary", "main", ptr(int64(18000)), ptr(100.0), ptr(at.Add(30*time.Second)), at, "cpa_passive")
	a := account{Windows: []window{w}}
	later := decorate(a, at.Add(time.Minute), 10*time.Minute)
	if later.Freshness != "stale" || later.Windows[0].State != "exhausted" || *later.Windows[0].AgeSeconds != 60 {
		t.Fatal("reset invented capacity or freshness")
	}
	merged := mergeWindows([]window{w}, []window{w})
	if !merged[0].ObservedAt.Equal(at) {
		t.Fatal("observation changed on reread")
	}
	older := w
	older.ObservedAt = ptr(at.Add(-time.Hour))
	older.UsedPercent = ptr(0.0)
	if *mergeWindows(merged, []window{older})[0].UsedPercent != 100 {
		t.Fatal("old observation overwrote new")
	}
}
