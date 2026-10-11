package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

type observation struct {
	ObservedAt *time.Time `json:"observed_at"`
	AgeSeconds *int64     `json:"age_seconds"`
	Freshness  string     `json:"freshness"`
}
type window struct {
	Key              string     `json:"key"`
	Group            string     `json:"group"`
	Scope            string     `json:"scope,omitempty"`
	Label            string     `json:"label"`
	DurationSeconds  *int64     `json:"duration_seconds"`
	UsedPercent      *float64   `json:"used_percent"`
	RemainingPercent *float64   `json:"remaining_percent"`
	ResetAt          *time.Time `json:"reset_at"`
	State            string     `json:"state"`
	Source           string     `json:"source"`
	observation
}
type routing struct {
	Available *bool      `json:"available"`
	State     string     `json:"state"`
	CheckedAt *time.Time `json:"checked_at"`
}
type refreshState struct {
	State         string     `json:"state"`
	Error         string     `json:"error,omitempty"`
	AttemptedAt   *time.Time `json:"attempted_at,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
}
type account struct {
	Provider        string       `json:"provider"`
	ID              string       `json:"account_id"`
	Plan            string       `json:"plan,omitempty"`
	Windows         []window     `json:"windows"`
	Routing         routing      `json:"routing"`
	Refresh         refreshState `json:"refresh"`
	LimitingWindow  string       `json:"limiting_window,omitempty"`
	AdditionalUsage string       `json:"additional_usage,omitempty"`
	observation
}
type providerView struct {
	Provider string    `json:"provider"`
	Accounts []account `json:"accounts"`
	Summary  summary   `json:"summary"`
}
type summary struct {
	AccountCount       int `json:"account_count"`
	RoutingAvailable   int `json:"routing_available"`
	RoutingUnavailable int `json:"routing_unavailable"`
	RoutingUnknown     int `json:"routing_unknown"`
	Fresh              int `json:"fresh_accounts"`
	Stale              int `json:"stale_accounts"`
	Unknown            int `json:"unknown_accounts"`
}
type response struct {
	SchemaVersion int            `json:"schema_version"`
	SharedPool    bool           `json:"shared_pool"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Providers     []providerView `json:"providers"`
	SnapshotState string         `json:"snapshot_state"`
	Warnings      []string       `json:"warnings"`
}
type storedAccount struct {
	Account        account   `json:"account"`
	Failures       int       `json:"failures"`
	NextAttempt    time.Time `json:"next_attempt"`
	RetryNotBefore time.Time `json:"retry_not_before"`
}
type cpaFile struct {
	Invalid     bool                 `json:"-"`
	AuthIndex   string               `json:"auth_index"`
	Provider    string               `json:"provider"`
	Disabled    bool                 `json:"disabled"`
	Unavailable bool                 `json:"unavailable"`
	Status      string               `json:"status"`
	Quota       signalSet            `json:"quota"`
	ModelQuotas map[string]signalSet `json:"model_quotas"`
	IDToken     struct {
		AccountID string `json:"chatgpt_account_id"`
		Plan      string `json:"plan_type"`
	} `json:"id_token"`
}
type signalSet struct {
	ObservedAt time.Time         `json:"observed_at"`
	Signals    map[string]string `json:"signals"`
}

func ptr[T any](v T) *T { return &v }
func opaqueID(salt []byte, provider, value string) string {
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(provider + "\x00" + value))
	return "acct_" + hex.EncodeToString(m.Sum(nil)[:16])
}
func freshness(at *time.Time, now time.Time, ttl time.Duration) observation {
	o := observation{ObservedAt: at, Freshness: "unknown"}
	if at == nil || at.IsZero() || at.After(now.Add(time.Minute)) {
		o.ObservedAt = nil
		return o
	}
	age := int64(now.Sub(*at).Seconds())
	if age < 0 {
		age = 0
	}
	o.AgeSeconds = &age
	o.Freshness = "fresh"
	if now.Sub(*at) >= ttl {
		o.Freshness = "stale"
	}
	return o
}
func decorate(a account, now time.Time, ttl time.Duration) account {
	a.Windows = append([]window{}, a.Windows...)
	a.observation = observation{Freshness: "unknown"}
	for i := range a.Windows {
		w := &a.Windows[i]
		w.observation = freshness(w.ObservedAt, now, ttl)
		if w.ResetAt != nil && !now.Before(*w.ResetAt) && w.Freshness == "fresh" {
			w.Freshness = "stale"
		}
		if w.Freshness == "unknown" {
			continue
		}
		if a.ObservedAt == nil || w.ObservedAt.Before(*a.ObservedAt) {
			a.observation = w.observation
		}
		if w.Freshness == "stale" {
			a.Freshness = "stale"
		}
	}
	for _, w := range a.Windows {
		if w.Freshness == "stale" {
			a.Freshness = "stale"
		}
	}
	return a
}
func mergeWindows(old, incoming []window) []window {
	byKey := map[string]window{}
	for _, w := range append(append([]window{}, old...), incoming...) {
		k := w.Group + "\x00" + w.Scope + "\x00" + w.Key
		prev, ok := byKey[k]
		if !ok || (w.ObservedAt != nil && (prev.ObservedAt == nil || !w.ObservedAt.Before(*prev.ObservedAt))) {
			byKey[k] = w
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]window, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out
}
