package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// Only dates are retained. Redemption IDs, identities, titles and raw payloads
// are deliberately absent from both the public view and persistent state.
type bankedCredit struct {
	GrantedAt *time.Time `json:"granted_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type bankedResets struct {
	AvailableCount *int           `json:"available_count"`
	Credits        []bankedCredit `json:"credits"`
	Refresh        refreshState   `json:"refresh"`
	observation
}

func normalizeBanked(data []byte, at time.Time) (*bankedResets, error) {
	var raw struct {
		AvailableCount *int `json:"available_count"`
		Credits        []struct {
			Status    string     `json:"status"`
			GrantedAt *time.Time `json:"granted_at"`
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"credits"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.AvailableCount == nil || *raw.AvailableCount < 0 || *raw.AvailableCount > 10000 || len(raw.Credits) > 1024 {
		return nil, errors.New("invalid banked resets observation")
	}
	out := &bankedResets{AvailableCount: raw.AvailableCount, Credits: []bankedCredit{}, observation: observation{ObservedAt: &at}}
	for _, c := range raw.Credits {
		if c.Status == "available" {
			out.Credits = append(out.Credits, bankedCredit{GrantedAt: c.GrantedAt, ExpiresAt: c.ExpiresAt})
		}
	}
	return out, nil
}

func decorateBanked(input *bankedResets, now time.Time, ttl time.Duration) *bankedResets {
	if input == nil {
		return &bankedResets{Credits: []bankedCredit{}, Refresh: refreshState{State: "idle"}, observation: observation{Freshness: "unknown"}}
	}
	out := *input
	out.Credits = append([]bankedCredit{}, input.Credits...)
	out.observation = freshness(input.ObservedAt, now, ttl)
	for _, c := range out.Credits {
		if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) && out.Freshness == "fresh" {
			out.Freshness = "stale"
		}
	}
	if out.Refresh.Error != "" && out.Freshness == "fresh" {
		out.Freshness = "stale"
	}
	return &out
}

// This operation is independent of usage refresh ownership. It calls only a
// fixed inventory GET, never the usage endpoint or a redemption/purchase route.
func (b *httpBackend) bankedResets(ctx context.Context, f cpaFile) (*bankedResets, *checkError) {
	if f.Provider != "codex" {
		return nil, &checkError{Code: "unsupported", Permanent: true}
	}
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json", "User-Agent": "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"}
	if f.IDToken.AccountID != "" {
		headers["Chatgpt-Account-Id"] = f.IDToken.AccountID
	}
	started := time.Now().UTC()
	resp, err := b.request(ctx, b.client, "POST", b.cfg.CPAURL+"/v0/management/api-call", b.cfg.ManagementKey, map[string]any{
		"auth_index": f.AuthIndex, "method": "GET", "url": "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", "header": headers,
	})
	if err != nil {
		return nil, &checkError{Code: "refresh_unavailable"}
	}
	if resp.StatusCode != 200 {
		retry := retryAfter(resp.Header.Get("Retry-After"), started)
		resp.Body.Close()
		return nil, &checkError{Code: "refresh_unavailable", RetryAt: retry}
	}
	var result struct {
		Status int                 `json:"status_code"`
		Header map[string][]string `json:"header"`
		Body   string              `json:"body"`
	}
	if boundedJSON(resp, &result) != nil {
		return nil, &checkError{Code: "invalid_observation", Permanent: true}
	}
	if result.Status != 200 {
		e := &checkError{Code: "refresh_unavailable", RetryAt: retryAfter(headerValue(result.Header, "Retry-After"), started)}
		switch result.Status {
		case 401, 403:
			e.Code, e.Permanent = "upstream_authentication_failed", true
		case 429:
			e.Code = "rate_limited"
		case 404, 405, 410:
			e.Code, e.Permanent = "unsupported", true
		}
		return nil, e
	}
	value, err := normalizeBanked([]byte(result.Body), started)
	if err != nil {
		return nil, &checkError{Code: "invalid_observation", Permanent: true}
	}
	return value, nil
}

func (s *service) refreshBankedOne(ctx context.Context) {
	if !s.cfg.CodexBankedResets || !s.bankedMu.TryLock() {
		return
	}
	defer s.bankedMu.Unlock()
	now := time.Now().UTC()
	s.mu.Lock()
	if s.snapshotFailed || now.Sub(s.snapshotAt) > 2*s.cfg.SnapshotEvery || s.persistenceFailed || now.Sub(s.lastInterest) > s.cfg.InterestFor {
		s.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(s.files))
	for id := range s.files {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	selected := ""
	for _, id := range ids {
		f, e := s.files[id], s.accounts[id]
		if f.Provider == "codex" && !f.Invalid && !f.Disabled && !now.Before(e.BankedNextAttempt) && !now.Before(e.RetryNotBefore) {
			selected = id
			break
		}
	}
	if selected == "" {
		s.mu.Unlock()
		return
	}
	entry, file := s.accounts[selected], s.files[selected]
	entry.Account.BankedResets = decorateBanked(entry.Account.BankedResets, now, s.cfg.BankedStale)
	entry.BankedNextAttempt = now.Add(s.cfg.BankedInterval)
	entry.Account.BankedResets.Refresh = refreshState{State: "running", AttemptedAt: &now, NextAttemptAt: ptr(entry.BankedNextAttempt)}
	s.accounts[selected] = entry
	if !s.saveLocked() {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	result, failure := s.upstream.bankedResets(ctx, file)
	finished := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.accounts[selected]
	if !exists {
		return
	}
	if failure == nil && result == nil {
		failure = &checkError{Code: "invalid_observation", Permanent: true}
	}
	refresh := refreshState{State: "succeeded", AttemptedAt: &now}
	if failure == nil {
		entry.Account.BankedResets = result
		entry.BankedFailures = 0
		entry.BankedNextAttempt = finished.Add(s.cfg.BankedInterval)
	} else {
		entry.BankedFailures++
		delay := s.cfg.BankedInterval
		for n := 1; n < entry.BankedFailures && delay < s.cfg.MaxBackoff; n++ {
			delay *= 2
		}
		if delay > s.cfg.MaxBackoff || failure.Permanent || entry.BankedFailures >= 5 {
			delay = s.cfg.MaxBackoff
		}
		entry.BankedNextAttempt = finished.Add(delay)
		if failure.RetryAt.After(entry.BankedNextAttempt) {
			entry.BankedNextAttempt = failure.RetryAt
		}
		refresh.State, refresh.Error = "backoff", failure.Code
	}
	refresh.NextAttemptAt = ptr(entry.BankedNextAttempt)
	entry.Account.BankedResets.Refresh = refresh
	s.accounts[selected] = entry
	s.saveLocked()
}
