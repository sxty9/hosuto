// Package hconfig is a service's reader for the holistic configuration standard — the sibling of
// the rights standard.
//
// Rights: permissions/<id>.json → /etc/holistic/permissions.d/<id>.json → privleg writes Linux
// groups → every daemon reads the groups live.
//
// Config: config/<id>.json → /etc/holistic/config.d/<id>.json declares the settings and their
// defaults → an admin edits them in the dashboard's central Configuration tab → the dashboard
// writes /var/lib/holistic/config/<id>.json → every daemon reads that file live.
//
// The same shape as the JWT secret and the rights groups: one file, one writer (the dashboard),
// many readers, no RPC and no push. A daemon therefore needs no restart when config changes, and a
// host with no values file simply runs on the manifest defaults.
package hconfig

import (
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	// ManifestDir is where each service drops its config manifest (installed by ./service setup).
	ManifestDir = "/etc/holistic/config.d"
	// ValuesDir is where the dashboard writes admin-edited values. Group-readable by `holistic`.
	ValuesDir = "/var/lib/holistic/config"
	// ttl bounds how stale a daemon's view of the config may be. Short enough that an admin's edit
	// takes effect while they are still looking at the tab.
	ttl = 5 * time.Second
)

// Setting is one declared knob.
type Setting struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Type      string   `json:"type"` // string | int | bool | enum | secret
	Default   any      `json:"default"`
	Options   []string `json:"options,omitempty"` // enum only
	Dangerous bool     `json:"dangerous,omitempty"`
}

// Category groups settings for display.
type Category struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Settings []Setting `json:"settings"`
}

// Manifest is the declared config surface of one service.
type Manifest struct {
	Service    string     `json:"service"`
	Version    int        `json:"version"`
	Categories []Category `json:"categories"`
}

// Config resolves a service's effective configuration: manifest defaults overlaid with the values
// the admin set. It re-reads both files on a short TTL, so it is safe to call on a request path.
type Config struct {
	service      string
	manifestPath string
	valuesPath   string

	mu   sync.Mutex
	at   time.Time
	eff  map[string]any
	seen bool
}

// New builds a reader for one service. Empty dirs fall back to the standard locations.
func New(service, manifestDir, valuesDir string) *Config {
	if manifestDir == "" {
		manifestDir = ManifestDir
	}
	if valuesDir == "" {
		valuesDir = ValuesDir
	}
	return &Config{
		service:      service,
		manifestPath: manifestDir + "/" + service + ".json",
		valuesPath:   valuesDir + "/" + service + ".json",
		eff:          map[string]any{},
	}
}

// refresh rebuilds the effective map. Caller holds the mutex.
//
// A missing or malformed file is never fatal: the daemon must boot and serve on defaults even if
// the dashboard has never written a values file (a fresh host) or an admin hand-corrupted one.
func (c *Config) refresh() {
	if c.seen && time.Since(c.at) < ttl {
		return
	}
	eff := map[string]any{}

	if b, err := os.ReadFile(c.manifestPath); err == nil {
		var m Manifest
		if json.Unmarshal(b, &m) == nil {
			for _, cat := range m.Categories {
				for _, s := range cat.Settings {
					if s.Default != nil {
						eff[s.ID] = s.Default
					}
				}
			}
		}
	}
	if b, err := os.ReadFile(c.valuesPath); err == nil {
		var v map[string]any
		if json.Unmarshal(b, &v) == nil {
			for k, val := range v {
				eff[k] = val
			}
		}
	}
	c.eff, c.at, c.seen = eff, time.Now(), true
}

func (c *Config) raw(id string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
	v, ok := c.eff[id]
	return v, ok
}

// String returns a string setting, or def if unset/wrong type.
func (c *Config) String(id, def string) string {
	v, ok := c.raw(id)
	if !ok {
		return def
	}
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// Int returns an int setting, or def if unset/unparseable.
//
// JSON numbers decode to float64, and an admin may well have typed "4096" into a text box, so both
// shapes are accepted rather than silently falling back to the default.
func (c *Config) Int(id string, def int) int {
	v, ok := c.raw(id)
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return def
}

// Bool returns a bool setting, or def if unset/wrong type.
func (c *Config) Bool(id string, def bool) bool {
	v, ok := c.raw(id)
	if !ok {
		return def
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		if p, err := strconv.ParseBool(b); err == nil {
			return p
		}
	}
	return def
}
