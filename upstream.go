package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"
)

type httpBackend struct {
	cfg    Config
	client *http.Client
	keeper *http.Client
}
type checkError struct {
	Code      string
	RetryAt   time.Time
	Permanent bool
}

func newBackend(cfg Config) *httpBackend {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxConnsPerHost = 40
	jar, _ := cookiejar.New(nil)
	redirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &httpBackend{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: redirect}, keeper: &http.Client{Timeout: 5 * time.Second, Transport: transport, Jar: jar, CheckRedirect: redirect}}
}
func (b *httpBackend) request(ctx context.Context, client *http.Client, method, url, key string, body any) (*http.Response, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, errors.New("invalid request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid request")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CPA-Usage-Keeper-Request", "fetch")
	}
	return client.Do(req)
}
func boundedJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if err != nil || len(data) > 8*1024*1024 {
		return errors.New("invalid response")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("invalid response")
	}
	return nil
}
func (b *httpBackend) authorize(ctx context.Context, key string) (bool, error) {
	resp, err := b.request(ctx, b.client, "GET", b.cfg.CPAURL+"/v1/models", key, nil)
	if err != nil {
		return false, errors.New("authorization unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return false, nil
	}
	if resp.StatusCode != 200 {
		return false, errors.New("authorization unavailable")
	}
	var value struct {
		Data   []json.RawMessage `json:"data"`
		Object string            `json:"object"`
	}
	if boundedJSON(resp, &value) != nil || value.Object != "list" || value.Data == nil {
		return false, errors.New("authorization unavailable")
	}
	return true, nil
}
func (b *httpBackend) snapshot(ctx context.Context) ([]cpaFile, error) {
	resp, err := b.request(ctx, b.client, "GET", b.cfg.CPAURL+"/v0/management/auth-files", b.cfg.ManagementKey, nil)
	if err != nil {
		return nil, errors.New("snapshot unavailable")
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, errors.New("snapshot unavailable")
	}
	var raw struct {
		Files []json.RawMessage `json:"files"`
	}
	if boundedJSON(resp, &raw) != nil || raw.Files == nil {
		return nil, errors.New("snapshot unavailable")
	}
	files := []cpaFile{}
	for _, data := range raw.Files {
		var f cpaFile
		if json.Unmarshal(data, &f) != nil {
			var identity struct {
				AuthIndex string `json:"auth_index"`
				Provider  string `json:"provider"`
			}
			if json.Unmarshal(data, &identity) != nil || identity.AuthIndex == "" {
				continue
			}
			f = cpaFile{AuthIndex: identity.AuthIndex, Provider: identity.Provider, Invalid: true}
		}
		files = append(files, f)
	}
	return files, nil
}
func (b *httpBackend) ownership(ctx context.Context) bool {
	if b.cfg.KeeperURL == "" {
		return false
	}
	read := func() (*http.Response, error) {
		return b.request(ctx, b.keeper, "GET", b.cfg.KeeperURL+"/api/v1/quota/auto-refresh/settings", "", nil)
	}
	resp, err := read()
	if err != nil {
		return false
	}
	if resp.StatusCode == 401 {
		resp.Body.Close()
		login, e := b.request(ctx, b.keeper, "POST", b.cfg.KeeperURL+"/api/v1/auth/login", "", map[string]string{"password": b.cfg.KeeperPassword})
		if e != nil {
			return false
		}
		status := login.StatusCode
		login.Body.Close()
		if status != 204 && status != 200 {
			return false
		}
		resp, err = read()
		if err != nil {
			return false
		}
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return false
	}
	var settings struct {
		Enabled *bool `json:"enabled"`
	}
	if boundedJSON(resp, &settings) != nil || settings.Enabled == nil || *settings.Enabled {
		return false
	}
	return true
}
func (b *httpBackend) check(ctx context.Context, f cpaFile) (account, *checkError) {
	url := "https://chatgpt.com/backend-api/wham/usage"
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json"}
	switch f.Provider {
	case "codex":
		headers["User-Agent"] = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
		if f.IDToken.AccountID != "" {
			headers["Chatgpt-Account-Id"] = f.IDToken.AccountID
		}
	case "claude":
		url = "https://api.anthropic.com/api/oauth/usage"
		headers["anthropic-beta"] = "oauth-2025-04-20"
	default:
		return account{}, &checkError{Code: "unsupported", Permanent: true}
	}
	started := time.Now().UTC()
	resp, err := b.request(ctx, b.client, "POST", b.cfg.CPAURL+"/v0/management/api-call", b.cfg.ManagementKey, map[string]any{"auth_index": f.AuthIndex, "method": "GET", "url": url, "header": headers})
	if err != nil {
		return account{}, &checkError{Code: "refresh_unavailable"}
	}
	if resp.StatusCode != 200 {
		retry := retryAfter(resp.Header.Get("Retry-After"), started)
		resp.Body.Close()
		return account{}, &checkError{Code: "refresh_unavailable", RetryAt: retry}
	}
	var result struct {
		Status int         `json:"status_code"`
		Header http.Header `json:"header"`
		Body   string      `json:"body"`
	}
	if boundedJSON(resp, &result) != nil {
		return account{}, &checkError{Code: "invalid_observation", Permanent: true}
	}
	if result.Status != 200 {
		e := &checkError{Code: "refresh_unavailable", RetryAt: retryAfter(headerValue(result.Header, "Retry-After"), time.Now())}
		switch result.Status {
		case 401, 403:
			e.Code = "upstream_authentication_failed"
			e.Permanent = true
		case 429:
			e.Code = "rate_limited"
		case 404, 405, 410:
			e.Code = "unsupported"
			e.Permanent = true
		}
		return account{}, e
	}
	a, err := normalizeActive(f.Provider, []byte(result.Body), started, b.cfg.Salt)
	if err != nil || len(a.Windows) == 0 {
		return account{}, &checkError{Code: "invalid_observation", Permanent: true}
	}
	return a, nil
}
func headerValue(h http.Header, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}
func retryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 && seconds <= 315360000 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at
	}
	return time.Time{}
}
