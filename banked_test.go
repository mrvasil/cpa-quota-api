package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBankedProjectionHasOnlyAvailableDates(t *testing.T) {
	at := time.Now().UTC()
	result, err := normalizeBanked([]byte(`{"available_count":2,"profile_user_id":"private-user","credits":[
		{"id":"redemption-secret","title":"private-title","status":"available","granted_at":"2030-01-01T00:00:00Z","expires_at":"2030-02-01T00:00:00Z"},
		{"id":"used-secret","status":"redeemed","expires_at":"2029-01-01T00:00:00Z"},
		{"status":"available","granted_at":null,"expires_at":null}]}`), at)
	if err != nil || *result.AvailableCount != 2 || len(result.Credits) != 2 || result.Credits[0].GrantedAt == nil || result.Credits[1].ExpiresAt != nil {
		t.Fatalf("projection failed: %+v %v", result, err)
	}
	encoded, _ := json.Marshal(result)
	for _, private := range []string{"secret", "private", "redeemed", "2029", "\"id\""} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private or unavailable field retained: %s", encoded)
		}
	}
	for _, data := range []string{`{}`, `{"available_count":null}`, `{"available_count":-1}`, `{"available_count":1.5}`, `{"available_count":"2"}`, `{"available_count":1,"credits":[{"status":"available","expires_at":"bad"}]}`} {
		if _, err := normalizeBanked([]byte(data), at); err == nil {
			t.Fatalf("malformed observation accepted: %s", data)
		}
	}
	zero, err := normalizeBanked([]byte(`{"available_count":0,"credits":null}`), at)
	if err != nil || zero.AvailableCount == nil || *zero.AvailableCount != 0 || zero.Credits == nil {
		t.Fatal("explicit zero confused with unknown")
	}
	if unknown := decorateBanked(nil, at, time.Hour); unknown.AvailableCount != nil || unknown.Freshness != "unknown" {
		t.Fatal("missing observation invented a balance")
	}
}

func TestBankedUpstreamCanOnlyReadFixedInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method    string            `json:"method"`
			URL       string            `json:"url"`
			AuthIndex string            `json:"auth_index"`
			Header    map[string]string `json:"header"`
			Data      any               `json:"data"`
		}
		if r.Method != "POST" || r.URL.Path != "/v0/management/api-call" || r.Header.Get("Authorization") != "Bearer admin" || json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("unexpected CPA request")
		}
		if req.Method != "GET" || req.URL != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" || req.AuthIndex != "account" || req.Header["Authorization"] != "Bearer $TOKEN$" || req.Data != nil {
			t.Error("inventory operation can mutate or select another endpoint")
		}
		writeJSON(w, 200, map[string]any{"status_code": 200, "body": `{"available_count":0,"credits":[]}`})
	}))
	defer server.Close()
	b := newBackend(Config{CPAURL: server.URL, ManagementKey: "admin"})
	result, failure := b.bankedResets(context.Background(), cpaFile{Provider: "codex", AuthIndex: "account"})
	if failure != nil || *result.AvailableCount != 0 {
		t.Fatalf("inventory read failed: %+v", failure)
	}
	if _, failure := b.bankedResets(context.Background(), cpaFile{Provider: "claude"}); failure == nil || failure.Code != "unsupported" {
		t.Fatal("unsupported provider contacted")
	}
}

func TestBankedErrorsAreSanitizedAndBudgeted(t *testing.T) {
	for _, code := range []int{429, 401, 404, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 200, map[string]any{"status_code": code, "header": map[string][]string{"retry-after": {"7200"}}, "body": `{"error":"private-token"}`})
			}))
			defer server.Close()
			_, failure := newBackend(Config{CPAURL: server.URL}).bankedResets(context.Background(), cpaFile{Provider: "codex"})
			if failure == nil || time.Until(failure.RetryAt) < 119*time.Minute {
				t.Fatal("error or Retry-After lost")
			}
			encoded, _ := json.Marshal(failure)
			if strings.Contains(string(encoded), "private") {
				t.Fatal("provider error leaked")
			}
		})
	}
}

func bankedTestService(t *testing.T) (*service, *fakeBackend) {
	t.Helper()
	at := time.Now().UTC()
	result, err := normalizeBanked([]byte(`{"available_count":1,"credits":[{"id":"private-redemption-id","status":"available","expires_at":"2099-01-01T00:00:00Z"}]}`), at)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{files: []cpaFile{{AuthIndex: "one", Provider: "codex", Status: "active"}}, bankedResult: result}
	f.valid.Store(true)
	s := testService(t, f)
	s.cfg.RefreshOwner = "keeper"
	s.cfg.CodexBankedResets = true
	s.cfg.BankedInterval = 15 * time.Minute
	s.cfg.BankedStale = 15 * time.Minute
	return s, f
}

func TestBankedCacheSurvivesSnapshotsConcurrencyAndRestart(t *testing.T) {
	s, f := bankedTestService(t)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() { defer wg.Done(); s.refreshBankedOne(context.Background()) }()
	}
	wg.Wait()
	if f.bankedCalls.Load() != 1 || f.calls.Load() != 0 {
		t.Fatal("banked or usage polling multiplied")
	}
	s.readSnapshot(context.Background())
	view := s.view(time.Now())
	if *view.Providers[0].Accounts[0].BankedResets.AvailableCount != 1 {
		t.Fatal("passive snapshot lost banked data")
	}
	state, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "state.json"))
	if err != nil || strings.Contains(string(state), "private-redemption-id") {
		t.Fatal("state contains redemption id")
	}
	restarted, err := newService(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.upstream = f
	restarted.lastInterest = time.Now()
	restarted.readSnapshot(context.Background())
	restarted.refreshBankedOne(context.Background())
	if f.bankedCalls.Load() != 1 {
		t.Fatal("restart bypassed inventory budget")
	}
	for range 10 {
		r := httptest.NewRequest("GET", "/v1/quota", nil)
		r.Header.Set("Authorization", "Bearer valid")
		w := httptest.NewRecorder()
		restarted.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "banked_resets") {
			t.Fatal("cached response missing")
		}
	}
	if f.bankedCalls.Load() != 1 {
		t.Fatal("client requests contacted inventory")
	}
}

func TestBankedFailureRetainsOldDatesAndPersistsBackoff(t *testing.T) {
	s, f := bankedTestService(t)
	s.refreshBankedOne(context.Background())
	for id, e := range s.accounts {
		e.BankedNextAttempt = time.Time{}
		s.accounts[id] = e
	}
	f.bankedFailure = &checkError{Code: "rate_limited", RetryAt: time.Now().Add(48 * time.Hour)}
	s.refreshBankedOne(context.Background())
	a := s.view(time.Now()).Providers[0].Accounts[0].BankedResets
	if *a.AvailableCount != 1 || a.Credits[0].ExpiresAt == nil || a.Freshness != "stale" || a.Refresh.Error != "rate_limited" || time.Until(*a.Refresh.NextAttemptAt) < 47*time.Hour {
		t.Fatalf("last good observation or backoff lost: %+v", a)
	}
	restarted, err := newService(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.upstream = f
	restarted.lastInterest = time.Now()
	restarted.readSnapshot(context.Background())
	restarted.refreshBankedOne(context.Background())
	if f.bankedCalls.Load() != 2 {
		t.Fatal("restart bypassed failure backoff")
	}
}

func TestBankedDisabledIdleAndInvalidAccountsNeverPoll(t *testing.T) {
	for _, mode := range []string{"disabled", "idle", "invalid", "account_disabled", "snapshot_failure", "storage_failure", "claude"} {
		t.Run(mode, func(t *testing.T) {
			s, f := bankedTestService(t)
			switch mode {
			case "disabled":
				s.cfg.CodexBankedResets = false
			case "idle":
				s.lastInterest = time.Now().Add(-time.Hour)
			case "snapshot_failure":
				s.snapshotFailed = true
			case "storage_failure":
				s.persistenceFailed = true
			default:
				for id, file := range s.files {
					if mode == "invalid" {
						file.Invalid = true
					}
					if mode == "account_disabled" {
						file.Disabled = true
					}
					if mode == "claude" {
						file.Provider = "claude"
					}
					s.files[id] = file
				}
			}
			s.refreshBankedOne(context.Background())
			if f.bankedCalls.Load() != 0 {
				t.Fatal("unexpected inventory poll")
			}
		})
	}
}

func TestBankedExpiryAndErrorsDoNotInventZeroOrFreshness(t *testing.T) {
	at := time.Now().UTC()
	old := at.Add(-time.Hour)
	input := &bankedResets{AvailableCount: ptr(1), Credits: []bankedCredit{{ExpiresAt: &old}}, observation: observation{ObservedAt: &at}}
	result := decorateBanked(input, at, time.Hour)
	if result.Freshness != "stale" || *result.AvailableCount != 1 || !result.ObservedAt.Equal(at) {
		t.Fatal("expiry invented balance or freshness")
	}
	if input.Freshness != "" {
		t.Fatal("decoration mutated cached data")
	}
}

func TestBankedConfigurationIsOptInAndCannotPollTooOften(t *testing.T) {
	t.Setenv("CPA_MANAGEMENT_KEY", "synthetic-test-key")
	t.Setenv("CPA_MANAGEMENT_KEY_FILE", "")
	t.Setenv("ACCOUNT_ID_SALT", strings.Repeat("ab", 32))
	t.Setenv("ACCOUNT_ID_SALT_FILE", "")
	t.Setenv("REFRESH_OWNER", "keeper")
	t.Setenv("CODEX_BANKED_RESETS", "false")
	t.Setenv("BANKED_RESETS_INTERVAL", "15m")
	t.Setenv("BANKED_RESETS_STALE_AFTER", "15m")
	cfg, err := loadConfig()
	if err != nil || cfg.CodexBankedResets || cfg.BankedInterval != 15*time.Minute {
		t.Fatal("stock-compatible defaults changed")
	}
	t.Setenv("CODEX_BANKED_RESETS", "true")
	if cfg, err = loadConfig(); err != nil || !cfg.CodexBankedResets || cfg.RefreshOwner != "keeper" {
		t.Fatal("inventory requires changing usage ownership")
	}
	for _, value := range []string{"1s", "14m", "0", "invalid"} {
		t.Setenv("BANKED_RESETS_INTERVAL", value)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("unsafe interval accepted: %s", value)
		}
	}
	t.Setenv("BANKED_RESETS_INTERVAL", "15m")
	t.Setenv("CODEX_BANKED_RESETS", "typo")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid opt-in accepted")
	}
}

type blockingBankedBackend struct {
	*fakeBackend
	started chan struct{}
	release chan struct{}
}

func (b *blockingBankedBackend) bankedResets(ctx context.Context, f cpaFile) (*bankedResets, *checkError) {
	close(b.started)
	select {
	case <-ctx.Done():
		return nil, &checkError{Code: "refresh_interrupted"}
	case <-b.release:
		return b.fakeBackend.bankedResets(ctx, f)
	}
}

func TestSlowBankedProviderDoesNotBlockHTTPAndNoRedemptionRouteExists(t *testing.T) {
	s, f := bankedTestService(t)
	b := &blockingBankedBackend{fakeBackend: f, started: make(chan struct{}), release: make(chan struct{})}
	s.upstream = b
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.run(ctx)
	r := httptest.NewRequest("GET", "/v1/quota", nil)
	r.Header.Set("Authorization", "Bearer valid")
	s.ServeHTTP(httptest.NewRecorder(), r)
	select {
	case <-b.started:
	case <-time.After(3 * time.Second):
		t.Fatal("background inventory did not start")
	}
	defer func() { cancel(); <-s.done }()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); s.ServeHTTP(w, r); done <- w }()
	select {
	case w := <-done:
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"available_count":null`) {
			t.Fatal("unknown balance not returned while provider waits")
		}
	case <-time.After(time.Second):
		t.Fatal("slow inventory blocked HTTP response")
	}
	for _, route := range []struct {
		method, path string
		code         int
	}{
		{"POST", "/v1/quota", 405}, {"POST", "/v1/quota/reset", 404}, {"GET", "/v1/quota?redeem=1", 400},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
		if w.Code != route.code {
			t.Fatal("reset/force operation accepted")
		}
	}
	close(b.release)
}
