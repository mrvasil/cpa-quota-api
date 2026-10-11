package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type backend interface {
	authorize(context.Context, string) (bool, error)
	snapshot(context.Context) ([]cpaFile, error)
	ownership(context.Context) bool
	check(context.Context, cpaFile) (account, *checkError)
}
type service struct {
	cfg               Config
	upstream          backend
	mu                sync.Mutex
	refreshMu         sync.Mutex
	guardNext         time.Time
	accounts          map[string]storedAccount
	files             map[string]cpaFile
	lastInterest      time.Time
	snapshotAt        time.Time
	snapshotFailed    bool
	ownershipOK       bool
	persistenceFailed bool
	wake              chan struct{}
	done              chan struct{}
	authSlots         chan struct{}
}
type persisted struct {
	Version  int                      `json:"version"`
	Accounts map[string]storedAccount `json:"accounts"`
}

func newService(cfg Config) (*service, error) {
	s := &service{cfg: cfg, upstream: newBackend(cfg), accounts: map[string]storedAccount{}, files: map[string]cpaFile{}, wake: make(chan struct{}, 1), done: make(chan struct{}), authSlots: make(chan struct{}, 32)}
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var p persisted
	if json.Unmarshal(data, &p) != nil || p.Version != 1 || p.Accounts == nil {
		return nil, errors.New("invalid state")
	}
	s.accounts = p.Accounts
	for k, entry := range s.accounts {
		entry.Account.Routing = routing{State: "unknown"}
		entry.Account.Windows = filterWindows(entry.Account.Provider, entry.Account.Windows)
		if entry.Account.Refresh.State == "running" {
			entry.Account.Refresh.State = "backoff"
			entry.Account.Refresh.Error = "refresh_interrupted"
		}
		s.accounts[k] = entry
	}
	return s, nil
}
func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path != "/v1/quota" {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	if r.URL.RawQuery != "" {
		writeJSON(w, 400, map[string]string{"error": "unsupported_query"})
		return
	}
	key := bearerKey(r)
	if key == "" {
		writeJSON(w, 401, map[string]string{"error": "invalid_credentials"})
		return
	}
	select {
	case s.authSlots <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "5")
		writeJSON(w, 503, map[string]string{"error": "temporarily_busy"})
		return
	}
	valid, err := s.upstream.authorize(r.Context(), key)
	<-s.authSlots
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "authorization_unavailable"})
		return
	}
	if !valid {
		writeJSON(w, 401, map[string]string{"error": "invalid_credentials"})
		return
	}
	s.mu.Lock()
	s.lastInterest = time.Now()
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	w.Header().Set("Retry-After", "30")
	writeJSON(w, 200, s.view(time.Now().UTC()))
}
func bearerKey(r *http.Request) string {
	if len(r.Header.Values("Authorization")) > 1 || len(r.Header.Values("X-API-Key")) > 1 {
		return ""
	}
	auth := r.Header.Get("Authorization")
	api := r.Header.Get("X-API-Key")
	if auth != "" && api != "" {
		return ""
	}
	if auth != "" {
		p := strings.Fields(auth)
		if len(p) != 2 || !strings.EqualFold(p[0], "Bearer") {
			return ""
		}
		api = p[1]
	}
	if len(api) > 4096 || strings.TrimSpace(api) != api || strings.ContainsAny(api, "\r\n\t ") {
		return ""
	}
	return api
}
func (s *service) run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.cfg.SnapshotEvery)
	defer ticker.Stop()
	s.readSnapshot(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		s.mu.Lock()
		interested := time.Since(s.lastInterest) <= s.cfg.InterestFor
		due := time.Since(s.snapshotAt) >= s.cfg.SnapshotEvery
		s.mu.Unlock()
		if !interested {
			continue
		}
		if due {
			s.readSnapshot(ctx)
		}
		s.refreshOne(ctx)
	}
}
func (s *service) readSnapshot(ctx context.Context) {
	files, err := s.upstream.snapshot(ctx)
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotAt = now
	s.snapshotFailed = err != nil
	if err != nil {
		return
	}
	next := map[string]storedAccount{}
	currentFiles := map[string]cpaFile{}
	for _, f := range files {
		if (f.Provider != "claude" && f.Provider != "codex") || f.AuthIndex == "" {
			continue
		}
		id := opaqueID(s.cfg.Salt, f.Provider, f.AuthIndex)
		a := normalizeFile(f, id, now, s.cfg.Salt)
		prev := s.accounts[id]
		sets := []signalSet{f.Quota}
		for _, set := range f.ModelQuotas {
			sets = append(sets, set)
		}
		for _, set := range sets {
			if set.ObservedAt.IsZero() || set.ObservedAt.After(now.Add(time.Minute)) {
				continue
			}
			h := lowerSignals(set.Signals)
			until := retryAfter(h["retry-after"], set.ObservedAt)
			if until.After(prev.RetryNotBefore) {
				prev.RetryNotBefore = until
			}
		}
		a.Windows = filterWindows(f.Provider, a.Windows)
		a.Windows = mergeWindows(filterWindows(f.Provider, prev.Account.Windows), a.Windows)
		if a.Plan == "" {
			a.Plan = prev.Account.Plan
		}
		a.Refresh = prev.Account.Refresh
		if a.Refresh.State == "" {
			a.Refresh.State = "idle"
		}
		prev.Account = a
		if s.cfg.RefreshOwner == "keeper" {
			prev.Account.Refresh.State = "external_owner"
		}
		next[id] = prev
		currentFiles[id] = f
	}
	s.accounts = next
	s.files = currentFiles
	s.saveLocked()
}
func (s *service) refreshOne(ctx context.Context) {
	if !s.refreshMu.TryLock() {
		return
	}
	defer s.refreshMu.Unlock()
	if s.cfg.RefreshOwner != "quota-api" {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	if s.snapshotFailed || now.Sub(s.snapshotAt) > 2*s.cfg.SnapshotEvery || s.persistenceFailed || now.Before(s.guardNext) || now.Sub(s.lastInterest) > s.cfg.InterestFor {
		s.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(s.accounts))
	for id := range s.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	selected := ""
	for _, id := range ids {
		entry := s.accounts[id]
		a := decorate(entry.Account, now, s.cfg.stale(entry.Account.Provider))
		f, ok := s.files[id]
		if !ok || f.Invalid || f.Disabled || now.Before(entry.NextAttempt) || now.Before(entry.RetryNotBefore) {
			continue
		}
		if needsRefresh(a) {
			selected = id
			break
		}
	}
	s.mu.Unlock()
	if selected == "" {
		return
	}
	owns := s.upstream.ownership(ctx)
	s.mu.Lock()
	s.ownershipOK = owns
	if !owns {
		s.guardNext = now.Add(time.Minute)
		s.mu.Unlock()
		return
	}
	entry := s.accounts[selected]
	f := s.files[selected]
	entry.Account.Refresh = refreshState{State: "running", AttemptedAt: &now}
	entry.NextAttempt = now.Add(s.cfg.interval(f.Provider))
	entry.Account.Refresh.NextAttemptAt = ptr(entry.NextAttempt)
	s.accounts[selected] = entry
	if !s.saveLocked() {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	result, checkErr := s.upstream.check(ctx, f)
	finished := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry = s.accounts[selected]
	if checkErr == nil {
		entry.Account.Windows = reconcileActiveWindows(entry.Account.Provider, entry.Account.Windows, result.Windows)
		if result.Plan != "" {
			entry.Account.Plan = result.Plan
		}
		if result.AdditionalUsage != "" {
			entry.Account.AdditionalUsage = result.AdditionalUsage
		}
		entry.Failures = 0
		entry.Account.Refresh.State = "succeeded"
		entry.Account.Refresh.Error = ""
		entry.NextAttempt = finished.Add(s.cfg.interval(f.Provider))
	} else {
		entry.Failures++
		delay := s.cfg.interval(f.Provider)
		for n := 1; n < entry.Failures && delay < s.cfg.MaxBackoff; n++ {
			delay *= 2
		}
		if delay > s.cfg.MaxBackoff {
			delay = s.cfg.MaxBackoff
		}
		if checkErr.Permanent || entry.Failures >= 5 {
			delay = s.cfg.MaxBackoff
		}
		entry.NextAttempt = finished.Add(delay)
		if checkErr.RetryAt.After(entry.NextAttempt) {
			entry.NextAttempt = checkErr.RetryAt
		}
		entry.RetryNotBefore = checkErr.RetryAt
		entry.Account.Refresh.State = "backoff"
		entry.Account.Refresh.Error = checkErr.Code
	}
	entry.Account.Refresh.NextAttemptAt = ptr(entry.NextAttempt)
	s.accounts[selected] = entry
	s.saveLocked()
}
func needsRefresh(a account) bool {
	if len(a.Windows) == 0 {
		return true
	}
	for _, w := range a.Windows {
		if w.Freshness != "fresh" {
			return true
		}
	}
	return false
}
func (s *service) saveLocked() bool {
	data, err := json.Marshal(persisted{Version: 1, Accounts: s.accounts})
	path := filepath.Join(s.cfg.StateDir, "state.json")
	if err == nil {
		var file *os.File
		file, err = os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err == nil {
			_, err = file.Write(data)
			if err == nil {
				err = file.Sync()
			}
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err == nil {
		dir, openErr := os.Open(s.cfg.StateDir)
		err = openErr
		if err == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
	}
	s.persistenceFailed = err != nil
	return err == nil
}
func (s *service) view(now time.Time) response {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := response{SchemaVersion: 1, SharedPool: true, GeneratedAt: now, Providers: []providerView{}, SnapshotState: "available", Warnings: []string{"Shared subscription pool; percentages are not additive. Observations do not guarantee future capacity or model routing success."}}
	if s.snapshotAt.IsZero() {
		out.SnapshotState = "unknown"
	} else if s.snapshotFailed || now.Sub(s.snapshotAt) > 2*s.cfg.SnapshotEvery {
		out.SnapshotState = "stale"
	}
	if s.snapshotFailed {
		out.Warnings = append(out.Warnings, "snapshot_unavailable")
	}
	if s.persistenceFailed {
		out.Warnings = append(out.Warnings, "state_storage_unavailable")
	}
	for _, provider := range []string{"codex", "claude"} {
		p := providerView{Provider: provider, Accounts: []account{}}
		for _, entry := range s.accounts {
			if entry.Account.Provider != provider {
				continue
			}
			a := entry.Account
			a.Windows = filterWindows(provider, a.Windows)
			a = decorate(a, now, s.cfg.stale(provider))
			if out.SnapshotState != "available" {
				a.Routing.Available = nil
				a.Routing.State = "unknown"
			}
			if s.cfg.RefreshOwner == "quota-api" && !s.ownershipOK && a.Refresh.State != "running" {
				a.Refresh.State = "ownership_unverified"
			}
			p.Accounts = append(p.Accounts, a)
			p.Summary.AccountCount++
			if a.Routing.Available == nil {
				p.Summary.RoutingUnknown++
			} else if *a.Routing.Available {
				p.Summary.RoutingAvailable++
			} else {
				p.Summary.RoutingUnavailable++
			}
			switch a.Freshness {
			case "fresh":
				p.Summary.Fresh++
			case "stale":
				p.Summary.Stale++
			default:
				p.Summary.Unknown++
			}
		}
		sort.Slice(p.Accounts, func(i, j int) bool { return p.Accounts[i].ID < p.Accounts[j].ID })
		out.Providers = append(out.Providers, p)
	}
	return out
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		status = 500
		data = []byte(`{"error":"internal_error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
