// HTTP-based manager for Prometheus http_sd and http_rule_files endpoints.
//
// Provides RESTful CRUD APIs to manage service discovery target groups and
// rule groups, persisted to JSON files on disk. Prometheus polls the read
// endpoints (/sd/{name}, /rules/{name}) via http_sd_config and
// http_rule_files respectively; any mutation through the management API
// is immediately visible at the next refresh_interval, triggering
// hot-reload when content changes (SHA256 comparison).
//
// Run:
//
//	go run .
//
// Management API (CRUD):
//
//	SD files:   GET/POST/PUT/DELETE /api/v1/sd/{name}
//	Rule files: GET/POST/PUT/DELETE /api/v1/rules/{name}
//	List:       GET /api/v1/sd | /api/v1/rules
//
// Prometheus read endpoints:
//
//	GET /sd/{name}     - http_sd target groups JSON array
//	GET /rules/{name}  - http_rule_files rule groups JSON array
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

const (
	addr       = ":9092"
	dataDir    = "data"
	sdSubdir   = "sd"
	ruleSubdir = "rules"
)

// ---------------------------------------------------------------------------
// Shared data types - field names mirror the JSON schema expected by Prometheus.
// ---------------------------------------------------------------------------

// sdTargetGroup mirrors the http_sd JSON node.
type sdTargetGroup struct {
	Labels  map[string]string `json:"labels,omitempty"`
	Targets []string          `json:"targets"`
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

// ruleGroup mirrors the http_rule_files JSON node.
type ruleGroup struct {
	Name     string        `json:"name"`
	Interval string        `json:"interval,omitempty"`
	Rules    []interface{} `json:"rules"`
}

// ---------------------------------------------------------------------------
// Store: thread-safe JSON-file-backed persistence for a single collection.
// ---------------------------------------------------------------------------

// store holds a collection of named JSON documents. Each document is either
// []sdTargetGroup or []ruleGroup, serialised to disk under dataDir/<sub>/<name>.json.
type store struct {
	mu  sync.RWMutex
	dir string
}

func newStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir %q: %w", dir, err)
	}
	return &store{dir: dir}, nil
}

// list returns the names (without .json suffix) of all documents.
func (s *store) list() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names, nil
}

// path returns the on-disk path for a named document.
func (s *store) path(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid name %q", name)
	}
	return filepath.Join(s.dir, name+".json"), nil
}

// load reads and unmarshals a document into dst.
func (s *store) load(name string, dst interface{}) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, err := s.path(name)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNotFound
		}
		return err
	}
	return json.Unmarshal(b, dst)
}

// save marshals and writes a document atomically.
func (s *store) save(name string, v interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// delete removes a document.
func (s *store) delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNotFound
		}
		return err
	}
	return nil
}

// exists reports whether a document exists.
func (s *store) exists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, err := s.path(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// ---------------------------------------------------------------------------
// Sentinel errors.
// ---------------------------------------------------------------------------

var errNotFound = errors.New("not found")

// ---------------------------------------------------------------------------
// HTTP handlers - SD files.
// ---------------------------------------------------------------------------

func listSD(c *gin.Context, st *store) {
	names, err := st.list()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"files": names})
}

func getSD(c *gin.Context, st *store) {
	name := c.Param("name")
	var groups []sdTargetGroup
	if err := st.load(name, &groups); err != nil {
		statusForError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "groups": groups})
}

func createSD(c *gin.Context, st *store) {
	name := c.Param("name")
	if st.exists(name) {
		c.JSON(http.StatusConflict, gin.H{"error": "file already exists; use PUT to update"})
		return
	}
	var groups []sdTargetGroup
	if err := c.ShouldBindJSON(&groups); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := st.save(name, groups); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"name": name, "groups": groups, "message": "created"})
}

func updateSD(c *gin.Context, st *store) {
	name := c.Param("name")
	var groups []sdTargetGroup
	if err := c.ShouldBindJSON(&groups); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := st.save(name, groups); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "groups": groups, "message": "updated"})
}

func deleteSD(c *gin.Context, st *store) {
	name := c.Param("name")
	if err := st.delete(name); err != nil {
		statusForError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "message": "deleted"})
}

// ---------------------------------------------------------------------------
// HTTP handlers - Rule files.
// ---------------------------------------------------------------------------

func listRules(c *gin.Context, st *store) {
	names, err := st.list()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"files": names})
}

func getRules(c *gin.Context, st *store) {
	name := c.Param("name")
	var groups []ruleGroup
	if err := st.load(name, &groups); err != nil {
		statusForError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "groups": groups})
}

func createRules(c *gin.Context, st *store) {
	name := c.Param("name")
	if st.exists(name) {
		c.JSON(http.StatusConflict, gin.H{"error": "file already exists; use PUT to update"})
		return
	}
	var groups []ruleGroup
	if err := c.ShouldBindJSON(&groups); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := st.save(name, groups); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"name": name, "groups": groups, "message": "created"})
}

func updateRules(c *gin.Context, st *store) {
	name := c.Param("name")
	var groups []ruleGroup
	if err := c.ShouldBindJSON(&groups); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := st.save(name, groups); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "groups": groups, "message": "updated"})
}

func deleteRules(c *gin.Context, st *store) {
	name := c.Param("name")
	if err := st.delete(name); err != nil {
		statusForError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "message": "deleted"})
}

// ---------------------------------------------------------------------------
// Prometheus read endpoints.
// ---------------------------------------------------------------------------

// sdRead serves /sd/{name} for http_sd_config. Returns the raw JSON array
// (no envelope) so Prometheus can unmarshal it directly.
func sdRead(c *gin.Context, st *store) {
	name := c.Param("name")
	var groups []sdTargetGroup
	if err := st.load(name, &groups); err != nil {
		statusForError(c, err)
		return
	}
	c.JSON(http.StatusOK, groups)
}

// rulesRead serves /rules/{name} for http_rule_files. Returns the raw JSON
// array (no envelope) so Prometheus can unmarshal it directly.
func rulesRead(c *gin.Context, st *store) {
	name := c.Param("name")
	var groups []ruleGroup
	if err := st.load(name, &groups); err != nil {
		statusForError(c, err)
		return
	}
	c.JSON(http.StatusOK, groups)
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func statusForError(c *gin.Context, err error) {
	if errors.Is(err, errNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// seedIfEmpty pre-populates the stores with example documents on first run
// so users can immediately see something working.
func seedIfEmpty(sd, rules *store) {
	if !sd.exists("default") {
		_ = sd.save("default", []sdTargetGroup{
			{Labels: map[string]string{"job": "demo"}, Targets: []string{"localhost:9090"}},
			{Labels: map[string]string{"job": "down-service"}, Targets: []string{"localhost:9000"}},
		})
	}
	if !rules.exists("default") {
		_ = rules.save("default", []ruleGroup{{
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
		}})
	}
}

// ---------------------------------------------------------------------------
// main.
// ---------------------------------------------------------------------------

func main() {
	sdDir := filepath.Join(dataDir, sdSubdir)
	ruleDir := filepath.Join(dataDir, ruleSubdir)
	sd, err := newStore(sdDir)
	if err != nil {
		log.Fatalf("sd store: %v", err)
	}
	rules, err := newStore(ruleDir)
	if err != nil {
		log.Fatalf("rules store: %v", err)
	}
	seedIfEmpty(sd, rules)

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Management API - SD files.
	api := r.Group("/api/v1")
	{
		api.GET("/sd", func(c *gin.Context) { listSD(c, sd) })
		api.GET("/sd/:name", func(c *gin.Context) { getSD(c, sd) })
		api.POST("/sd/:name", func(c *gin.Context) { createSD(c, sd) })
		api.PUT("/sd/:name", func(c *gin.Context) { updateSD(c, sd) })
		api.DELETE("/sd/:name", func(c *gin.Context) { deleteSD(c, sd) })

		api.GET("/rules", func(c *gin.Context) { listRules(c, rules) })
		api.GET("/rules/:name", func(c *gin.Context) { getRules(c, rules) })
		api.POST("/rules/:name", func(c *gin.Context) { createRules(c, rules) })
		api.PUT("/rules/:name", func(c *gin.Context) { updateRules(c, rules) })
		api.DELETE("/rules/:name", func(c *gin.Context) { deleteRules(c, rules) })
	}

	// Prometheus read endpoints - raw JSON arrays, no envelope.
	r.GET("/sd/:name", func(c *gin.Context) { sdRead(c, sd) })
	r.GET("/rules/:name", func(c *gin.Context) { rulesRead(c, rules) })

	// Convenience: dashboard root shows links.
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service": "http-sd-rule-manager",
			"endpoints": gin.H{
				"sd_management":         "/api/v1/sd, /api/v1/sd/{name}",
				"rules_management":      "/api/v1/rules, /api/v1/rules/{name}",
				"prometheus_sd_read":    "GET /sd/{name}",
				"prometheus_rules_read": "GET /rules/{name}",
			},
		})
	})

	log.Printf("http-sd-rule-manager listening on %s", addr)
	log.Printf("  data dir: %s", dataDir)
	log.Printf("  Management API:")
	log.Printf("    SD:    GET/POST/PUT/DELETE /api/v1/sd/{name}")
	log.Printf("    Rules: GET/POST/PUT/DELETE /api/v1/rules/{name}")
	log.Printf("  Prometheus read endpoints:")
	log.Printf("    SD:    GET /sd/{name}")
	log.Printf("    Rules: GET /rules/{name}")
	if err := r.Run(addr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
