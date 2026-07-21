// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httprules

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/promql/parser"
)

// mutableBody is a tiny mutex-protected string holder used by test servers
// to allow the body to be swapped between requests without racy access.
type mutableBody struct {
	mu sync.RWMutex
	v  string
}

func (b *mutableBody) Load() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.v
}

func (b *mutableBody) Store(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.v = s
}

// goodRules is a JSON array of rule groups mirroring the YAML rule file schema.
const goodRules = `[{
  "name": "group1",
  "interval": "30s",
  "rules": [
    {"record": "job:http_inprogress_requests:sum", "expr": "sum by (job) (http_inprogress_requests)"},
    {"alert": "HighRequestLatency", "expr": "rate(request_latency_seconds_sum[5m]) > 0.5", "for": "10m", "labels": {"severity": "page"}}
  ]
}]`

// goodRulesV2 differs from goodRules so the SHA256 hash changes.
const goodRulesV2 = `[{
  "name": "group1",
  "interval": "30s",
  "rules": [
    {"record": "job:http_inprogress_requests:sum", "expr": "sum by (job) (http_inprogress_requests)"},
    {"alert": "HighRequestLatency", "expr": "rate(request_latency_seconds_sum[5m]) > 0.9", "for": "10m", "labels": {"severity": "page"}}
  ]
}]`

const invalidJSONRules = `[{"name": "group1", "interval":`

const invalidValidationRules = `[{
  "name": "",
  "rules": [{"record": "foo", "expr": "up"}]
}]`

func newTestProvider(t *testing.T, configs []HTTPRuleFileConfig) *Provider {
	t.Helper()
	p, err := NewProvider(configs, parser.NewParser(parser.Options{}), model.UTF8Validation, slog.New(slog.NewTextHandler(&discardWriter{}, nil)))
	require.NoError(t, err)
	return p
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestProviderLoadInitialFetch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, goodRules)
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, []HTTPRuleFileConfig{
		{URL: ts.URL, HTTPClientConfig: config.DefaultHTTPClientConfig, RefreshInterval: model.Duration(time.Hour)},
	})

	rgs, errs := p.Load(ts.URL)
	require.Empty(t, errs)
	require.Len(t, rgs.Groups, 1)
	require.Equal(t, "group1", rgs.Groups[0].Name)
	require.Len(t, rgs.Groups[0].Rules, 2)
}

func TestProviderLoadReturnsCachedWithoutFetch(t *testing.T) {
	var fetchCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, goodRules)
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, []HTTPRuleFileConfig{
		{URL: ts.URL, HTTPClientConfig: config.DefaultHTTPClientConfig, RefreshInterval: model.Duration(time.Hour)},
	})

	// First Load triggers a synchronous fetch.
	_, errs := p.Load(ts.URL)
	require.Empty(t, errs)
	first := fetchCount.Load()

	// Second Load should be served from cache without a new HTTP request.
	rgs, errs := p.Load(ts.URL)
	require.Empty(t, errs)
	require.Equal(t, first, fetchCount.Load(), "second Load should not fetch again")
	require.Len(t, rgs.Groups, 1)
}

func TestProviderLoadUnknownURL(t *testing.T) {
	p := newTestProvider(t, []HTTPRuleFileConfig{
		{URL: "http://example.invalid/rules.json", HTTPClientConfig: config.DefaultHTTPClientConfig, RefreshInterval: model.Duration(time.Hour)},
	})
	rgs, errs := p.Load("http://other.example/rules.json")
	require.Nil(t, rgs)
	require.Len(t, errs, 1)
	require.ErrorContains(t, errs[0], "unknown HTTP rule endpoint")
}

func TestProviderLoadFetchFailureReturnsEmpty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, []HTTPRuleFileConfig{
		{URL: ts.URL, HTTPClientConfig: config.DefaultHTTPClientConfig, RefreshInterval: model.Duration(time.Hour)},
	})

	rgs, errs := p.Load(ts.URL)
	require.Empty(t, errs, "initial fetch failure returns no error so reload can continue")
	require.NotNil(t, rgs)
	require.Empty(t, rgs.Groups)
}

func TestProviderRefreshTriggersOnUpdateOnContentChange(t *testing.T) {
	body := &mutableBody{}
	body.Store(goodRules)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body.Load())
	}))
	t.Cleanup(ts.Close)

	cfg := HTTPRuleFileConfig{
		URL:               ts.URL,
		HTTPClientConfig:  config.DefaultHTTPClientConfig,
		RefreshInterval:   model.Duration(50 * time.Millisecond),
	}
	p := newTestProvider(t, []HTTPRuleFileConfig{cfg})

	// Prime the cache with an initial Load.
	_, errs := p.Load(ts.URL)
	require.Empty(t, errs)

	var updateCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	go p.Run(ctx, func() { updateCount.Add(1) })

	// Wait briefly, then change the body. The next refresh should fire onUpdate.
	time.Sleep(150 * time.Millisecond)
	body.Store(goodRulesV2)

	// Wait long enough for at least one refresh cycle to detect the change.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if updateCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Greater(t, updateCount.Load(), int32(0), "onUpdate should fire when content changes")

	// The cache should now reflect the new content.
	rgs, _ := p.Load(ts.URL)
	require.Len(t, rgs.Groups, 1)
	expr := rgs.Groups[0].Rules[1].Expr
	require.Contains(t, expr, "0.9", "cached rules should be the updated version")
}

func TestProviderRefreshNoUpdateOnIdenticalContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, goodRules)
	}))
	t.Cleanup(ts.Close)

	cfg := HTTPRuleFileConfig{
		URL:               ts.URL,
		HTTPClientConfig:  config.DefaultHTTPClientConfig,
		RefreshInterval:   model.Duration(50 * time.Millisecond),
	}
	p := newTestProvider(t, []HTTPRuleFileConfig{cfg})

	// Prime the cache.
	_, errs := p.Load(ts.URL)
	require.Empty(t, errs)

	var updateCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t.Cleanup(cancel)
	go p.Run(ctx, func() { updateCount.Add(1) })

	// Wait several refresh cycles; identical content should not fire onUpdate.
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(0), updateCount.Load(), "onUpdate should not fire when content is unchanged")
}

func TestProviderRefreshKeepsCachedRulesOnFetchFailure(t *testing.T) {
	body := &mutableBody{}
	body.Store(goodRules)
	status := atomic.Int32{}
	status.Store(http.StatusOK)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status.Load() != http.StatusOK {
			http.Error(w, "boom", int(status.Load()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body.Load())
	}))
	t.Cleanup(ts.Close)

	cfg := HTTPRuleFileConfig{
		URL:               ts.URL,
		HTTPClientConfig:  config.DefaultHTTPClientConfig,
		RefreshInterval:   model.Duration(50 * time.Millisecond),
	}
	p := newTestProvider(t, []HTTPRuleFileConfig{cfg})

	// Prime the cache.
	rgs, errs := p.Load(ts.URL)
	require.Empty(t, errs)
	require.Len(t, rgs.Groups, 1)

	var updateCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t.Cleanup(cancel)
	go p.Run(ctx, func() { updateCount.Add(1) })

	// Break the server. Subsequent refreshes should fail but keep cached rules.
	status.Store(http.StatusInternalServerError)
	time.Sleep(300 * time.Millisecond)

	require.Equal(t, int32(0), updateCount.Load(), "onUpdate should not fire on fetch failure")

	// Load should still return the cached rules.
	rgs2, errs := p.Load(ts.URL)
	require.Empty(t, errs)
	require.Len(t, rgs2.Groups, 1, "cached rules should still be served after fetch failure")
}

func TestProviderRejectsNonJSONContentType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, goodRules)
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, []HTTPRuleFileConfig{
		{URL: ts.URL, HTTPClientConfig: config.DefaultHTTPClientConfig, RefreshInterval: model.Duration(time.Hour)},
	})
	rgs, errs := p.Load(ts.URL)
	require.Empty(t, errs)
	require.Empty(t, rgs.Groups, "non-JSON content type should yield no rules on initial fetch")
}

func TestProviderRejectsInvalidJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, invalidJSONRules)
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, []HTTPRuleFileConfig{
		{URL: ts.URL, HTTPClientConfig: config.DefaultHTTPClientConfig, RefreshInterval: model.Duration(time.Hour)},
	})
	rgs, errs := p.Load(ts.URL)
	require.Empty(t, errs, "invalid JSON returns no error so other endpoints can still load")
	require.Empty(t, rgs.Groups)
}

func TestProviderRejectsInvalidRuleGroups(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, invalidValidationRules)
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, []HTTPRuleFileConfig{
		{URL: ts.URL, HTTPClientConfig: config.DefaultHTTPClientConfig, RefreshInterval: model.Duration(time.Hour)},
	})
	rgs, errs := p.Load(ts.URL)
	require.Empty(t, errs)
	require.Empty(t, rgs.Groups, "invalid rule groups should yield no cached rules")
}

func TestProviderAcceptsJSONWithCharset(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, goodRules)
	}))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, []HTTPRuleFileConfig{
		{URL: ts.URL, HTTPClientConfig: config.DefaultHTTPClientConfig, RefreshInterval: model.Duration(time.Hour)},
	})
	rgs, errs := p.Load(ts.URL)
	require.Empty(t, errs)
	require.Len(t, rgs.Groups, 1)
}

func TestProviderJSONSchemaMatchesYAMLFieldNames(t *testing.T) {
	// Sanity check that the JSON schema uses the same field names as the YAML
	// rule file format (name, interval, rules, record, alert, expr, for, labels).
	var groups []rulefmt.RuleGroup
	require.NoError(t, json.Unmarshal([]byte(goodRules), &groups))
	require.Len(t, groups, 1)
	require.Equal(t, "group1", groups[0].Name)
	require.Len(t, groups[0].Rules, 2)
	require.Equal(t, "job:http_inprogress_requests:sum", groups[0].Rules[0].Record)
	require.Equal(t, "HighRequestLatency", groups[0].Rules[1].Alert)
	require.Equal(t, "10m", groups[0].Rules[1].For.String())
	require.Equal(t, "page", groups[0].Rules[1].Labels["severity"])
}
