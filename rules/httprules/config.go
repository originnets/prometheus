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
	"errors"
	"net/url"
	"time"

	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
)

// DefaultHTTPRuleFileConfig is the default HTTP rule file configuration.
var DefaultHTTPRuleFileConfig = HTTPRuleFileConfig{
	HTTPClientConfig: config.DefaultHTTPClientConfig,
	RefreshInterval:  model.Duration(60 * time.Second),
}

// HTTPRuleFileConfig is the configuration for fetching rules from an HTTP endpoint.
// The endpoint must return a JSON array of rule groups, where each group has the
// same field names as a YAML rule group (name, interval, rules, labels, ...).
type HTTPRuleFileConfig struct {
	HTTPClientConfig config.HTTPClientConfig `yaml:",inline"`
	RefreshInterval  model.Duration          `yaml:"refresh_interval,omitempty"`
	URL              string                  `yaml:"url"`
}

// Name returns the name of the config.
func (*HTTPRuleFileConfig) Name() string { return "http_rule_files" }

// SetDirectory joins any relative file paths with dir.
func (c *HTTPRuleFileConfig) SetDirectory(dir string) {
	c.HTTPClientConfig.SetDirectory(dir)
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *HTTPRuleFileConfig) UnmarshalYAML(unmarshal func(any) error) error {
	*c = DefaultHTTPRuleFileConfig
	type plain HTTPRuleFileConfig
	if err := unmarshal((*plain)(c)); err != nil {
		return err
	}
	if c.URL == "" {
		return errors.New("URL is missing")
	}
	parsedURL, err := url.Parse(c.URL)
	if err != nil {
		return err
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return errors.New("URL scheme must be 'http' or 'https'")
	}
	if parsedURL.Host == "" {
		return errors.New("host is missing in URL")
	}
	return c.HTTPClientConfig.Validate()
}
