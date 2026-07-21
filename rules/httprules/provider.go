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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/version"

	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/promql/parser"
)

var (
	userAgent          = version.PrometheusUserAgent()
	errEndpointUnknown = errors.New("unknown HTTP rule endpoint")
)

// Provider periodically fetches rule groups from a set of HTTP endpoints,
// caches the most recently successfully fetched and validated set per URL,
// and invokes a callback whenever the cached set changes.
//
// The provider is safe for concurrent use. The Load method is intended to be
// called from the rule manager's group loader; Run starts the polling
// goroutines and blocks until the context is cancelled.
type Provider struct {
	endpoints           []*endpoint
	parser              parser.Parser
	nameValidationScheme model.ValidationScheme
	logger              *slog.Logger

	// updateMu serializes invocations of the onUpdate callback. This prevents
	// concurrent reloads when multiple endpoints detect changes at the same time.
	updateMu sync.Mutex
}

type endpoint struct {
	cfg    HTTPRuleFileConfig
	client *http.Client
	logger *slog.Logger
	parser parser.Parser
	nameValidationScheme model.ValidationScheme

	mu     sync.RWMutex
	cached *rulefmt.RuleGroups
	hash   string
}

// NewProvider creates a new Provider for the given configurations.
// Each config gets its own HTTP client built from its HTTPClientConfig.
func NewProvider(
	configs []HTTPRuleFileConfig,
	promqlParser parser.Parser,
	nameValidationScheme model.ValidationScheme,
	logger *slog.Logger,
) (*Provider, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if promqlParser == nil {
		promqlParser = parser.NewParser(parser.Options{})
	}
	p := &Provider{
		parser:               promqlParser,
		nameValidationScheme: nameValidationScheme,
		logger:               logger,
	}
	for _, cfg := range configs {
		client, err := config.NewClientFromConfig(cfg.HTTPClientConfig, "http_rule_files")
		if err != nil {
			return nil, fmt.Errorf("error creating HTTP client for %s: %w", cfg.URL, err)
		}
		client.Timeout = time.Duration(cfg.RefreshInterval)
		ep := &endpoint{
			cfg:                  cfg,
			client:               client,
			logger:               logger,
			parser:               promqlParser,
			nameValidationScheme: nameValidationScheme,
		}
		p.endpoints = append(p.endpoints, ep)
	}
	return p, nil
}

// Run starts polling all configured HTTP endpoints. It blocks until ctx is
// cancelled. The onUpdate callback is invoked (serially) whenever the set of
// rules from any endpoint changes; a nil callback is ignored.
func (p *Provider) Run(ctx context.Context, onUpdate func()) {
	var wg sync.WaitGroup
	for _, ep := range p.endpoints {
		wg.Add(1)
		go func(ep *endpoint) {
			defer wg.Done()
			ep.run(ctx, func() {
				if onUpdate == nil {
					return
				}
				p.updateMu.Lock()
				defer p.updateMu.Unlock()
				onUpdate()
			})
		}(ep)
	}
	wg.Wait()
}

// Load returns the cached rule groups for the given URL.
//
// If no successful fetch has happened yet, Load performs a synchronous fetch
// so that the very first rule-manager update can see the current rules. On
// fetch/parse failure with no prior cache, an empty RuleGroups is returned
// with no error, so a single broken endpoint cannot break the whole rule
// reload. Subsequent successful polls will trigger an onUpdate that re-runs
// the rule reload with the freshly cached content.
//
// An unknown URL returns a nil RuleGroups and an error, mirroring the
// rulefmt.ParseFile convention for missing files.
func (p *Provider) Load(url string) (*rulefmt.RuleGroups, []error) {
	for _, ep := range p.endpoints {
		if ep.cfg.URL == url {
			return ep.load()
		}
	}
	return nil, []error{fmt.Errorf("%w: %s", errEndpointUnknown, url)}
}

func (ep *endpoint) load() (*rulefmt.RuleGroups, []error) {
	ep.mu.RLock()
	if ep.cached != nil {
		cached := ep.cached
		ep.mu.RUnlock()
		return cached, nil
	}
	ep.mu.RUnlock()

	// No cache yet: perform a synchronous fetch to populate it so the initial
	// rule-manager update can see the rules from this endpoint.
	rgs, hash, err := ep.fetchAndParse(context.Background())
	if err != nil {
		ep.logger.Warn("Error fetching HTTP rules on initial load; no rules cached",
			"url", ep.cfg.URL, "err", err)
		return &rulefmt.RuleGroups{}, nil
	}

	ep.mu.Lock()
	// Another goroutine may have populated the cache concurrently; keep the
	// existing cache in that case to avoid clobbering the hash tracking.
	if ep.cached == nil {
		ep.cached = rgs
		ep.hash = hash
	}
	cached := ep.cached
	ep.mu.Unlock()
	return cached, nil
}

func (ep *endpoint) run(ctx context.Context, onUpdate func()) {
	// Initial refresh. If it succeeds and produces new content, onUpdate is
	// called which triggers a rule reload with the freshly cached groups.
	ep.refresh(ctx, onUpdate)

	ticker := time.NewTicker(time.Duration(ep.cfg.RefreshInterval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ep.refresh(ctx, onUpdate)
		case <-ctx.Done():
			return
		}
	}
}

func (ep *endpoint) refresh(ctx context.Context, onUpdate func()) {
	rgs, hash, err := ep.fetchAndParse(ctx)
	if err != nil {
		ep.logger.Warn("Error refreshing HTTP rules; keeping previous rules",
			"url", ep.cfg.URL, "err", err)
		return
	}

	ep.mu.Lock()
	if ep.hash == hash {
		ep.mu.Unlock()
		return
	}
	ep.cached = rgs
	ep.hash = hash
	ep.mu.Unlock()

	if onUpdate != nil {
		onUpdate()
	}
}

// fetchAndParse performs a single HTTP fetch, validates the content type,
// computes a SHA256 hash of the body, parses the JSON body into rule groups
// and validates them. The returned hash is the hex-encoded SHA256 of the raw
// response body, allowing change detection even when parsing later fails.
func (ep *endpoint) fetchAndParse(ctx context.Context) (*rulefmt.RuleGroups, string, error) {
	req, err := http.NewRequest(http.MethodGet, ep.cfg.URL, http.NoBody)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := ep.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, "", err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("server returned HTTP status %s", resp.Status)
	}

	if !isJSONContentType(resp.Header.Get("Content-Type")) {
		return nil, "", fmt.Errorf("unsupported content type %q; expected application/json", resp.Header.Get("Content-Type"))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	// JSON schema: top-level array of rule groups, with field names mirroring
	// the YAML rule file format (name, interval, rules, labels, ...).
	var groups []rulefmt.RuleGroup
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, "", fmt.Errorf("error parsing JSON rule groups: %w", err)
	}

	rgs := &rulefmt.RuleGroups{Groups: groups}
	if errs := rgs.ValidateGroups(ep.nameValidationScheme, ep.parser); len(errs) > 0 {
		return nil, "", fmt.Errorf("invalid rule groups from %s: %w", ep.cfg.URL, errors.Join(errs...))
	}

	return rgs, hash, nil
}

// isJSONContentType reports whether the given Content-Type header value
// represents application/json (optionally with a charset parameter).
func isJSONContentType(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	// Allow "application/json" and "application/json; charset=utf-8" (case-insensitive).
	lower := strings.ToLower(v)
	if lower == "application/json" {
		return true
	}
	return strings.HasPrefix(lower, "application/json;")
}
