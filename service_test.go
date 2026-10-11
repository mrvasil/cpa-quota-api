package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	valid       atomic.Bool
	authFail    atomic.Bool
	calls       atomic.Int32
	files       []cpaFile
	snapshotErr error
	owns        bool
	result      account
	failure     *checkError
}

func (f *fakeBackend) authorize(context.Context, string) (bool, error) {
	if f.authFail.Load() {
		return false, errors.New("private raw error")
	}
	return f.valid.Load(), nil
}
func (f *fakeBackend) snapshot(context.Context) ([]cpaFile, error) { return f.files, f.snapshotErr }
func (f *fakeBackend) ownership(context.Context) bool              { return f.owns }
func (f *fakeBackend) check(context.Context, cpaFile) (account, *checkError) {
	f.calls.Add(1)
	return f.result, f.failure
}
func testConfig(t *testing.T) Config {
	return Config{CPAURL: "http://127.0.0.1", Salt: []byte(strings.Repeat("s", 32)), StateDir: t.TempDir(), RefreshOwner: "quota-api", SnapshotEvery: time.Second, InterestFor: time.Minute, CodexInterval: 15 * time.Minute, ClaudeInterval: 15 * time.Minute, CodexStale: 10 * time.Minute, ClaudeStale: 10 * time.Minute, MaxBackoff: 24 * time.Hour}
}
func testService(t *testing.T, f *fakeBackend) *service {
	t.Helper()
	s, err := newService(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	s.upstream = f
	s.lastInterest = time.Now()
	s.readSnapshot(context.Background())
	return s
}
func TestRevocationAndFailClosedWithoutAuthorizationCache(t *testing.T) {
	f := &fakeBackend{owns: true}
	f.valid.Store(true)
	s := testService(t, f)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/quota", nil)
		r.Header.Set("Authorization", "Bearer valid-key")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if request().Code != 200 {
		t.Fatal("valid key denied")
	}
	f.valid.Store(false)
	if request().Code != 401 {
		t.Fatal("revoked key retained access")
	}
	f.authFail.Store(true)
	w := request()
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("authorization failed open or leaked error")
	}
}
func TestConcurrentClientsDoNotMultiplyRefreshes(t *testing.T) {
	f := &fakeBackend{owns: true, files: []cpaFile{{AuthIndex: "one", Provider: "codex", Status: "active"}}, failure: &checkError{Code: "rate_limited", RetryAt: time.Now().Add(48 * time.Hour)}}
	f.valid.Store(true)
	s := testService(t, f)
	done := make(chan struct{}, 100)
	for range 100 {
		go func() {
			r := httptest.NewRequest("GET", "/v1/quota", nil)
			r.Header.Set("Authorization", "Bearer valid")
			s.ServeHTTP(httptest.NewRecorder(), r)
			s.refreshOne(context.Background())
			done <- struct{}{}
		}()
	}
	for range 100 {
		<-done
	}
	if f.calls.Load() != 1 {
		t.Fatalf("provider calls=%d", f.calls.Load())
	}
	for _, entry := range s.accounts {
		if entry.Account.Refresh.Error != "rate_limited" || time.Until(entry.NextAttempt) < 47*time.Hour {
			t.Fatal("retry guidance lost")
		}
	}
	restarted, err := newService(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.upstream = f
	restarted.lastInterest = time.Now()
	restarted.readSnapshot(context.Background())
	restarted.refreshOne(context.Background())
	if f.calls.Load() != 1 {
		t.Fatal("restart bypassed backoff")
	}
}
func TestFreshPassiveAndIdleClientsNeverActivelyPoll(t *testing.T) {
	f := &fakeBackend{owns: true, files: []cpaFile{{AuthIndex: "one", Provider: "codex", Status: "active", Quota: signalSet{ObservedAt: time.Now(), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "50", "X-Codex-Primary-Window-Minutes": "300"}}}}}
	s := testService(t, f)
	s.refreshOne(context.Background())
	if f.calls.Load() != 0 {
		t.Fatal("fresh observation actively polled")
	}
	s.mu.Lock()
	for id, e := range s.accounts {
		e.Account.Windows = nil
		s.accounts[id] = e
	}
	s.lastInterest = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	s.refreshOne(context.Background())
	if f.calls.Load() != 0 {
		t.Fatal("idle service polled")
	}
}
func TestKeeperOwnershipConflictStopsRefresh(t *testing.T) {
	f := &fakeBackend{files: []cpaFile{{AuthIndex: "one", Provider: "claude"}}}
	s := testService(t, f)
	s.refreshOne(context.Background())
	if f.calls.Load() != 0 || s.ownershipOK {
		t.Fatal("ownership conflict ignored")
	}
	s.cfg.RefreshOwner = "keeper"
	f.owns = true
	s.refreshOne(context.Background())
	if f.calls.Load() != 0 {
		t.Fatal("external owner polled")
	}
}
func TestSnapshotFailureRetainsAgeAndDoesNotInventRouting(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	f := &fakeBackend{files: []cpaFile{{AuthIndex: "one", Provider: "codex", Status: "active", Quota: signalSet{ObservedAt: at, Signals: map[string]string{"X-Codex-Primary-Used-Percent": "100", "X-Codex-Primary-Window-Minutes": "300"}}}}}
	s := testService(t, f)
	f.snapshotErr = errors.New("private file path")
	s.readSnapshot(context.Background())
	view := s.view(time.Now())
	a := view.Providers[0].Accounts[0]
	if view.SnapshotState != "stale" || a.Freshness != "stale" || !a.Windows[0].ObservedAt.Equal(at) || a.Routing.Available != nil || a.Windows[0].State != "exhausted" {
		t.Fatal("stale retention failed")
	}
}
func TestIncreasingBackoffAndLastObservationSurvivesFailure(t *testing.T) {
	f := &fakeBackend{owns: true, files: []cpaFile{{AuthIndex: "one", Provider: "claude"}}, failure: &checkError{Code: "refresh_unavailable"}}
	s := testService(t, f)
	var previous time.Duration
	for i := 0; i < 4; i++ {
		s.refreshOne(context.Background())
		for id, e := range s.accounts {
			delay := time.Until(e.NextAttempt)
			if delay <= previous {
				t.Fatal("backoff did not increase")
			}
			previous = delay
			e.NextAttempt = time.Time{}
			s.accounts[id] = e
		}
	}
}
func TestReadOnlySurfaceAndPrivacyHeaders(t *testing.T) {
	f := &fakeBackend{}
	f.valid.Store(true)
	s := testService(t, f)
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"POST", "/v1/quota", 405}, {"GET", "/v1/quota?force=1", 400}, {"GET", "/v0/management/api-call", 404}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(tc.method, tc.path, nil)
		s.ServeHTTP(w, r)
		if w.Code != tc.code || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected route behavior: %+v", tc)
		}
	}
}
func TestStateLockPreventsSecondRefreshOwner(t *testing.T) {
	dir := t.TempDir()
	first, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockState(dir); err == nil {
		second.Close()
		t.Fatal("second owner acquired state directory")
	}
}
