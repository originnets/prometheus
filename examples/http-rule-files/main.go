// Example HTTP server that serves Prometheus alerting rules for http_rule_files.
//
// Run:
//
//	go run .
//
// Then point Prometheus at it:
//
//	http_rule_files:
//	  - url: http://localhost:9091/rules
//	    refresh_interval: 15s
package main

import (
	"log"
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// ruleGroup mirrors the JSON schema expected by Prometheus http_rule_files.
// Field names match the YAML rule file format (name, interval, rules, ...).
type ruleGroup struct {
	Name     string        `json:"name"`
	Interval string        `json:"interval"`
	Rules    []interface{} `json:"rules"`
}

// alertingRule mirrors the alerting rule node in a rule group.
type alertingRule struct {
	Alert       string            `json:"alert"`
	Expr        string            `json:"expr"`
	For         string            `json:"for,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// recordingRule mirrors the recording rule node in a rule group.
type recordingRule struct {
	Record string            `json:"record"`
	Expr   string            `json:"expr"`
	Labels map[string]string `json:"labels,omitempty"`
}

// variant 0 and variant 1 are two different rule sets so the SHA256 hash
// changes when toggling, which triggers Prometheus to hot-reload.
var variant atomic.Int32

func currentRules() []ruleGroup {
	v := variant.Load()
	switch v {
	case 0:
		return []ruleGroup{{
			Name:     "service-health",
			Interval: "30s",
			Rules: []interface{}{
				alertingRule{
					Alert: "ServiceDown",
					Expr:  "up == 0",
					For:   "5m",
					Labels: map[string]string{
						"severity": "critical",
					},
					Annotations: map[string]string{
						"summary":     "Service {{ $labels.instance }} is down",
						"description": "{{ $labels.instance }} of job {{ $labels.job }} has been down for more than 5 minutes.",
					},
				},
			},
		},
			{
				Name:     "service-health1",
				Interval: "30s",
				Rules: []interface{}{
					alertingRule{
						Alert: "ServiceDown1",
						Expr:  "up == 1",
						For:   "5m",
						Labels: map[string]string{
							"severity": "critical",
						},
						Annotations: map[string]string{
							"summary":     "Service {{ $labels.instance }} is down",
							"description": "{{ $labels.instance }} of job {{ $labels.job }} has been down for more than 5 minutes.",
						},
					},
				},
			},
		}
	case 1:
		return []ruleGroup{{
			Name:     "service-health",
			Interval: "30s",
			Rules: []interface{}{
				alertingRule{
					Alert: "ServiceDown",
					Expr:  "up == 0",
					For:   "1m",
					Labels: map[string]string{
						"severity": "warning",
					},
					Annotations: map[string]string{
						"summary":     "Service {{ $labels.instance }} is down (fast)",
						"description": "{{ $labels.instance }} of job {{ $labels.job }} has been down for more than 1 minute.",
					},
				},
				recordingRule{
					Record: "job:up_ratio",
					Expr:   "avg by (job) (up)",
					Labels: map[string]string{
						"team": "sre",
					},
				},
			},
		}}
	default:
		return []ruleGroup{{
			Name:     "service-health",
			Interval: "30s",
			Rules: []interface{}{
				alertingRule{
					Alert: "ServiceDown",
					Expr:  "up == 0",
					For:   "5m",
					Labels: map[string]string{
						"severity": "critical",
					},
				},
			},
		}}
	}
}

func main() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// GET /rules returns the rule groups as a JSON array. This is the
	// endpoint Prometheus polls periodically via http_rule_files.
	r.GET("/rules", func(c *gin.Context) {
		c.JSON(http.StatusOK, currentRules())
	})

	// GET /toggle switches between variant 0 and variant 1 so the response
	// body (and thus its SHA256 hash) changes, which triggers Prometheus to
	// hot-reload the rules without a restart or /-/reload call.
	r.GET("/toggle", func(c *gin.Context) {
		old := variant.Load()
		variant.Store(1 - old)
		c.JSON(http.StatusOK, gin.H{
			"previous_variant": old,
			"current_variant":  variant.Load(),
			"message":          "rules changed; Prometheus should hot-reload within the next refresh interval",
		})
	})

	// GET /status reports the current variant and the rules being served.
	r.GET("/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"variant": variant.Load(),
			"groups":  currentRules(),
		})
	})

	addr := ":9091"
	log.Printf("http_rule_files test server listening on %s", addr)
	log.Printf("  GET /rules   - rule groups consumed by Prometheus http_rule_files")
	log.Printf("  GET /toggle  - switch rule variant to trigger hot-reload")
	log.Printf("  GET /status  - inspect current variant and rules")
	if err := r.Run(addr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
