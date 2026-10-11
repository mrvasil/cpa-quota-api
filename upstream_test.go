package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestActiveRequestIsFixedReadOnlyAndPreservesProxyPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/api-call" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer admin" {
			t.Error("unexpected request")
		}
		var req map[string]any
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("invalid request")
		}
		if req["method"] != "GET" || req["url"] != "https://api.anthropic.com/api/oauth/usage" || req["auth_index"] != "account" {
			t.Error("caller-controlled refresh")
		}
		if _, ok := req["proxy_url"]; ok {
			t.Error("proxy policy overridden")
		}
		if _, ok := req["data"]; ok {
			t.Error("unexpected mutation body")
		}
		headers := req["header"].(map[string]any)
		if headers["Authorization"] != "Bearer $TOKEN$" {
			t.Error("OAuth token handling changed")
		}
		writeJSON(w, 200, map[string]any{"status_code": 200, "body": `{"five_hour":{"utilization":1,"resets_at":"2030-01-01T00:00:00Z"}}`})
	}))
	defer server.Close()
	b := newBackend(Config{CPAURL: server.URL, ManagementKey: "admin"})
	a, e := b.check(context.Background(), cpaFile{Provider: "claude", AuthIndex: "account"})
	if e != nil || len(a.Windows) != 1 || *a.Windows[0].UsedPercent != 1 {
		t.Fatalf("active normalization failed: %+v %+v", a, e)
	}
}
func TestKeeperGuardRequiresExplicitDisabledSetting(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			if r.Header.Get("X-CPA-Usage-Keeper-Request") != "fetch" {
				t.Error("missing request intent")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "authenticated", Path: "/"})
			w.WriteHeader(204)
			return
		}
		if _, err := r.Cookie("session"); err != nil {
			w.WriteHeader(401)
			return
		}
		writeJSON(w, 200, map[string]bool{"enabled": enabled.Load()})
	}))
	defer server.Close()
	b := newBackend(Config{KeeperURL: server.URL, KeeperPassword: "password"})
	if b.ownership(context.Background()) {
		t.Fatal("enabled Keeper permitted second owner")
	}
	enabled.Store(false)
	if !b.ownership(context.Background()) {
		t.Fatal("disabled Keeper rejected")
	}
}
func TestProviderErrorsAreSanitizedAndRetryAfterPreserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status_code": 429, "header": map[string][]string{"retry-after": {"7200"}}, "body": `{"error":"secret@example.test /private/token"}`})
	}))
	defer server.Close()
	b := newBackend(Config{CPAURL: server.URL})
	_, e := b.check(context.Background(), cpaFile{Provider: "codex"})
	if e == nil || e.Code != "rate_limited" || time.Until(e.RetryAt) < 119*time.Minute {
		t.Fatal("retry guidance lost")
	}
	encoded, _ := json.Marshal(e)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("raw provider error leaked")
	}
}
func TestRedirectCannotLeakClientOrAdminCredentials(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer source.Close()
	b := newBackend(Config{CPAURL: source.URL})
	valid, err := b.authorize(context.Background(), "secret")
	if err == nil || valid || targetCalls.Load() != 0 {
		t.Fatal("redirect followed")
	}
}
