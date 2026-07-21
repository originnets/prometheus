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
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v2"
)

func TestHTTPRuleFileConfigUnmarshalYAML(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantErr   bool
		wantURL   string
		wantRef   model.Duration
	}{
		{
			name:    "valid minimal",
			yaml:    "url: http://example.com/rules.json\n",
			wantURL: "http://example.com/rules.json",
			wantRef: model.Duration(60 * time.Second), // default
		},
		{
			name:    "valid with refresh_interval",
			yaml:    "url: https://example.com/rules.json\nrefresh_interval: 30s\n",
			wantURL: "https://example.com/rules.json",
			wantRef: model.Duration(30 * time.Second),
		},
		{
			name:    "valid with basic_auth",
			yaml:    "url: http://example.com/rules.json\nbasic_auth:\n  username: alice\n  password: secret\n",
			wantURL: "http://example.com/rules.json",
			wantRef: model.Duration(60 * time.Second),
		},
		{
			name:    "missing url",
			yaml:    "refresh_interval: 30s\n",
			wantErr: true,
		},
		{
			name:    "bad scheme ftp",
			yaml:    "url: ftp://example.com/rules.json\n",
			wantErr: true,
		},
		{
			name:    "missing host",
			yaml:    "url: http:///rules.json\n",
			wantErr: true,
		},
		{
			name:    "bearer_token and bearer_token_file both set",
			yaml:    "url: http://example.com/rules.json\nbearer_token: foo\nbearer_token_file: /etc/secrets/token\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c HTTPRuleFileConfig
			err := yaml.UnmarshalStrict([]byte(tc.yaml), &c)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantURL, c.URL)
			require.Equal(t, tc.wantRef, c.RefreshInterval)
		})
	}
}

func TestHTTPRuleFileConfigSetDirectory(t *testing.T) {
	c := HTTPRuleFileConfig{
		HTTPClientConfig: config.HTTPClientConfig{
			BearerTokenFile: "token.txt",
		},
	}
	c.SetDirectory(filepath.Join("etc", "prometheus"))
	require.Equal(t, filepath.Join("etc", "prometheus", "token.txt"), c.HTTPClientConfig.BearerTokenFile)
}
