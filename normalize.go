package main

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

func normalizeFile(f cpaFile, id string, now time.Time, salt []byte) account {
	a := account{Provider: f.Provider, ID: id, Plan: safePlan(f.IDToken.Plan), Windows: []window{}, Routing: routing{State: "unknown", CheckedAt: ptr(now)}}
	switch {
	case f.Disabled:
		a.Routing.State = "disabled"
		a.Routing.Available = ptr(false)
	case f.Unavailable:
		a.Routing.State = "unavailable"
		a.Routing.Available = ptr(false)
	case f.Status == "active" || f.Status == "ok":
		a.Routing.State = "available"
		a.Routing.Available = ptr(true)
	case f.Status == "error":
		a.Routing.State = "error"
		a.Routing.Available = ptr(false)
	}
	a.Windows = mergeWindows(a.Windows, normalizeSignals(f.Provider, f.Quota, salt))
	h := lowerSignals(f.Quota.Signals)
	if p := safePlan(h["x-codex-plan-type"]); p != "" {
		a.Plan = p
	}
	switch h["anthropic-ratelimit-unified-representative-claim"] {
	case "five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet":
		a.LimitingWindow = h["anthropic-ratelimit-unified-representative-claim"]
	}
	switch h["anthropic-ratelimit-unified-overage-status"] {
	case "allowed", "allowed_warning", "rejected":
		a.AdditionalUsage = h["anthropic-ratelimit-unified-overage-status"]
	}
	return a
}
func lowerSignals(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}
func normalizeSignals(provider string, set signalSet, salt []byte) []window {
	if set.ObservedAt.IsZero() || set.ObservedAt.After(time.Now().Add(time.Minute)) {
		return nil
	}
	h := lowerSignals(set.Signals)
	if provider == "claude" {
		return claudeSignals(h, set.ObservedAt, salt)
	}
	out := []window{}
	prefixes := map[string]bool{"x-codex-": true}
	for k := range h {
		for _, role := range []string{"primary", "secondary"} {
			for _, suffix := range []string{"used-percent", "window-minutes", "reset-at", "reset-after-seconds"} {
				tail := role + "-" + suffix
				if strings.HasPrefix(k, "x-codex-") && strings.HasSuffix(k, tail) {
					prefixes[strings.TrimSuffix(k, tail)] = true
				}
			}
		}
	}
	for _, prefix := range sortedKeys(prefixes) {
		group := "main"
		if prefix != "x-codex-" {
			name := h[prefix+"limit-name"]
			if name == "" {
				name = strings.TrimSuffix(strings.TrimPrefix(prefix, "x-codex-"), "-")
			}
			group = privateName(salt, "limit", name)
			if prefix == "x-codex-code-review-" {
				group = "code_review"
			}
		}
		for _, role := range []string{"primary", "secondary"} {
			p := prefix + role + "-"
			seconds := durationNumber(h[p+"window-minutes"], 60)
			if raw, ok := h[p+"window-minutes"]; ok && seconds == nil && strings.TrimSpace(raw) == "0" {
				continue
			}
			used := percentString(h[p+"used-percent"], 1)
			reset := unixString(h[p+"reset-at"])
			if reset == nil {
				reset = relativeReset(h[p+"reset-after-seconds"], set.ObservedAt)
			}
			if used == nil && seconds == nil && reset == nil {
				continue
			}
			w := newWindow(role, group, seconds, used, reset, set.ObservedAt, "cpa_passive")
			if h[prefix+"allowed"] == "false" || h[prefix+"limit-reached"] == "true" {
				w.State = "limited"
			}
			out = append(out, w)
		}
	}
	return out
}
func claudeSignals(h map[string]string, at time.Time, salt []byte) []window {
	names := map[string]bool{}
	const prefix = "anthropic-ratelimit-unified-"
	for k := range h {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		name := strings.TrimPrefix(k, prefix)
		for _, suffix := range []string{"-utilization", "-reset", "-status"} {
			if strings.HasSuffix(name, suffix) {
				n := strings.TrimSuffix(name, suffix)
				if n == "5h" || n == "7d" || strings.HasPrefix(n, "7d_") {
					names[n] = true
				}
			}
		}
	}
	out := []window{}
	for _, n := range sortedKeys(names) {
		key, group, seconds := "seven_day", "main", int64(604800)
		if n == "5h" {
			key = "five_hour"
			seconds = 18000
		} else if n != "7d" {
			group = privateName(salt, "claude-scope", "seven_day_"+strings.TrimPrefix(n, "7d_"))
		}
		p := prefix + n + "-"
		w := newWindow(key, group, &seconds, percentString(h[p+"utilization"], 100), unixString(h[p+"reset"]), at, "cpa_passive")
		if group != "main" {
			w.Label = claudeScopeLabel(strings.TrimPrefix(n, "7d_"))
		}
		switch h[p+"status"] {
		case "rejected":
			w.State = "exhausted"
		case "allowed_warning":
			w.State = "warning"
		case "allowed":
			if w.UsedPercent == nil {
				w.State = "reported"
			}
		}
		out = append(out, w)
	}
	return out
}
func normalizeActive(provider string, data []byte, at time.Time, salt []byte) (account, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return account{}, errors.New("invalid observation")
	}
	a := account{Provider: provider, Windows: []window{}}
	if provider == "claude" {
		var limits []struct {
			Kind    string   `json:"kind"`
			Percent *float64 `json:"percent"`
			Reset   string   `json:"resets_at"`
			Scope   struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		}
		structured := json.Unmarshal(raw["limits"], &limits) == nil && len(limits) > 0
		if structured {
			for _, limit := range limits {
				key, group, seconds := "seven_day", "main", int64(604800)
				switch limit.Kind {
				case "session":
					key, seconds = "five_hour", 18000
				case "weekly_all":
				case "weekly_scoped":
					name := strings.ToLower(strings.TrimSpace(limit.Scope.Model.DisplayName))
					if name == "" {
						continue
					}
					group = privateName(salt, "claude-scope", "seven_day_"+name)
				default:
					continue
				}
				if validPercent(limit.Percent) == nil && isoTime(limit.Reset) == nil {
					continue
				}
				w := newWindow(key, group, &seconds, validPercent(limit.Percent), isoTime(limit.Reset), at, "active_check")
				if limit.Kind == "weekly_scoped" {
					w.Label = claudeScopeLabel(limit.Scope.Model.DisplayName)
				}
				a.Windows = append(a.Windows, w)
			}
		} else {
			for _, name := range sortedKeys(raw) {
				if name != "five_hour" && name != "seven_day" && !strings.HasPrefix(name, "seven_day_") {
					continue
				}
				var v struct {
					Utilization *float64 `json:"utilization"`
					ResetsAt    string   `json:"resets_at"`
				}
				if string(raw[name]) == "null" || json.Unmarshal(raw[name], &v) != nil || (v.Utilization == nil && v.ResetsAt == "") {
					continue
				}
				seconds := int64(604800)
				key, group := "seven_day", "main"
				if name == "five_hour" {
					seconds, key = 18000, "five_hour"
				} else if name != "seven_day" {
					group = privateName(salt, "claude-scope", name)
				}
				w := newWindow(key, group, &seconds, validPercent(v.Utilization), isoTime(v.ResetsAt), at, "active_check")
				if group != "main" {
					w.Label = claudeScopeLabel(strings.TrimPrefix(name, "seven_day_"))
				}
				a.Windows = append(a.Windows, w)
			}
		}
		var extra struct {
			Enabled *bool `json:"is_enabled"`
		}
		if json.Unmarshal(raw["extra_usage"], &extra) == nil && extra.Enabled != nil {
			a.AdditionalUsage = "disabled"
			if *extra.Enabled {
				a.AdditionalUsage = "enabled"
			}
		}
		return a, nil
	}
	var plan string
	_ = json.Unmarshal(raw["plan_type"], &plan)
	a.Plan = safePlan(plan)
	for _, pair := range []struct{ field, group string }{{"rate_limit", "main"}, {"code_review_rate_limit", "code_review"}} {
		a.Windows = append(a.Windows, activeCodexRate(raw[pair.field], pair.group, at)...)
	}
	var additional []struct {
		Name    string          `json:"limit_name"`
		Feature string          `json:"metered_feature"`
		Rate    json.RawMessage `json:"rate_limit"`
	}
	if json.Unmarshal(raw["additional_rate_limits"], &additional) == nil {
		for _, v := range additional {
			name := v.Name
			if name == "" {
				name = v.Feature
			}
			if name != "" {
				a.Windows = append(a.Windows, activeCodexRate(v.Rate, privateName(salt, "limit", name), at)...)
			}
		}
	} else {
		var groups map[string]json.RawMessage
		if json.Unmarshal(raw["additional_rate_limits"], &groups) == nil {
			for _, name := range sortedKeys(groups) {
				a.Windows = append(a.Windows, activeCodexRate(groups[name], privateName(salt, "limit", name), at)...)
			}
		}
	}
	return a, nil
}
func activeCodexRate(data json.RawMessage, group string, at time.Time) []window {
	var rate struct {
		Primary   json.RawMessage `json:"primary_window"`
		Secondary json.RawMessage `json:"secondary_window"`
		Allowed   *bool           `json:"allowed"`
		Reached   *bool           `json:"limit_reached"`
	}
	if json.Unmarshal(data, &rate) != nil {
		return nil
	}
	out := []window{}
	for _, item := range []struct {
		role string
		data json.RawMessage
	}{{"primary", rate.Primary}, {"secondary", rate.Secondary}} {
		if len(item.data) == 0 || string(item.data) == "null" {
			continue
		}
		var v struct {
			Used    *float64 `json:"used_percent"`
			Seconds *int64   `json:"limit_window_seconds"`
			Reset   *int64   `json:"reset_at"`
			After   *int64   `json:"reset_after_seconds"`
		}
		if json.Unmarshal(item.data, &v) != nil {
			continue
		}
		if v.Seconds != nil && *v.Seconds == 0 {
			continue
		}
		if v.Seconds != nil && (*v.Seconds < 0 || *v.Seconds > 315360000) {
			v.Seconds = nil
		}
		var reset *time.Time
		if v.Reset != nil {
			reset = unixString(strconv.FormatInt(*v.Reset, 10))
		}
		if reset == nil && v.After != nil {
			reset = relativeReset(strconv.FormatInt(*v.After, 10), at)
		}
		used := validPercent(v.Used)
		if used == nil && v.Seconds == nil && reset == nil {
			continue
		}
		w := newWindow(item.role, group, v.Seconds, used, reset, at, "active_check")
		if (rate.Reached != nil && *rate.Reached) || (rate.Allowed != nil && !*rate.Allowed) {
			w.State = "limited"
		}
		out = append(out, w)
	}
	return out
}
func newWindow(key, group string, seconds *int64, used *float64, reset *time.Time, at time.Time, source string) window {
	w := window{Key: key, Group: group, Label: "unknown_duration", DurationSeconds: seconds, UsedPercent: used, ResetAt: reset, State: "unknown", Source: source, observation: observation{ObservedAt: &at}}
	if seconds != nil {
		w.Label = "custom"
		switch *seconds {
		case 18000:
			w.Label = "five_hour"
		case 604800:
			w.Label = "weekly"
		case 86400:
			w.Label = "daily"
		}
	}
	if used != nil {
		w.RemainingPercent = ptr(math.Max(0, 100-*used))
		w.State = "reported"
		if *used >= 100 {
			w.State = "exhausted"
		}
	}
	return w
}
func claudeScopeLabel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "fable":
		return "7-day Fable"
	case "sonnet":
		return "7-day Sonnet"
	case "opus":
		return "7-day Opus"
	case "haiku":
		return "7-day Haiku"
	default:
		return "7-day scoped limit"
	}
}

func safePlan(value string) string {
	switch strings.ToLower(value) {
	case "free", "plus", "pro", "team", "business", "enterprise", "education", "max", "max_5x", "max_20x":
		return strings.ToLower(value)
	}
	return ""
}
func privateName(salt []byte, kind, name string) string {
	return kind + "_" + strings.TrimPrefix(opaqueID(salt, kind, strings.ToLower(name)), "acct_")[:16]
}
func filterWindows(provider string, windows []window) []window {
	out := make([]window, 0, len(windows))
	for _, w := range windows {
		if w.Scope != "" || (w.UsedPercent == nil && w.ResetAt == nil) {
			continue
		}
		if provider == "codex" && w.Group != "main" {
			continue
		}
		if provider == "claude" && w.Group != "main" && (!strings.HasPrefix(w.Group, "claude-scope_") || w.Key != "seven_day" || w.Source != "active_check") {
			continue
		}
		out = append(out, w)
	}
	return out
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func validPercent(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > 100 {
		return nil
	}
	return v
}
func percentString(value string, multiplier float64) *float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return nil
	}
	n *= multiplier
	return validPercent(&n)
}
func durationNumber(value string, multiplier int64) *int64 {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || n > 315360000/multiplier {
		return nil
	}
	return ptr(n * multiplier)
}
func unixString(value string) *time.Time {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || n > 253402300799 {
		return nil
	}
	return ptr(time.Unix(n, 0).UTC())
}
func relativeReset(value string, at time.Time) *time.Time {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || n > 315360000 || at.IsZero() {
		return nil
	}
	return ptr(at.Add(time.Duration(n) * time.Second))
}
func isoTime(value string) *time.Time {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return ptr(at.UTC())
}
