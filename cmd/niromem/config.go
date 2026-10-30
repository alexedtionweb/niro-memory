package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the server configuration file (JSON). ${VAR} references are
// expanded from the environment before parsing, so secrets stay out of files.
type Config struct {
	// Env is "development" or "production". Production refuses the dev
	// console and dev authentication.
	Env    string `json:"env"`
	Listen string `json:"listen"`

	Store    Choice `json:"store"`
	Vector   Choice `json:"vector"`
	Blob     Choice `json:"blob"`
	Cache    Choice `json:"cache"`
	Embedder Choice `json:"embedder"`

	// Rules is a path to a rules file (see configs/rules.example.json).
	Rules string `json:"rules"`
	Auth  struct {
		// Dev trusts the X-Tenant header and grants admin (development only).
		Dev  bool      `json:"dev"`
		Keys []KeyConf `json:"keys"`
	} `json:"auth"`
	Worker struct {
		// Inline runs a worker inside `serve` (embedded deployments).
		Inline      bool `json:"inline"`
		Concurrency int  `json:"concurrency"`
	} `json:"worker"`
	DebounceMS int `json:"debounce_ms"`
}

// KeyConf mirrors api.Key.
type KeyConf struct {
	Key    string   `json:"key,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	Tenant string   `json:"tenant"`
	User   string   `json:"user_id,omitempty"`
	Agent  string   `json:"agent_id,omitempty"`
	Roles  []string `json:"roles,omitempty"`
	Admin  bool     `json:"admin,omitempty"`
}

// Choice selects a connector by name; its options live under that name.
//
//	"store": {"use": "sqlite", "sqlite": {"path": "niromem.db"}}
type Choice struct {
	Use     string
	Options map[string]json.RawMessage
}

// UnmarshalJSON reads {"use": name, name: {...}, other: {...}}.
func (c *Choice) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if u, ok := m["use"]; ok {
		if err := json.Unmarshal(u, &c.Use); err != nil {
			return err
		}
	}
	delete(m, "use")
	c.Options = m
	return nil
}

// Decode reads the options of the selected connector into v.
func (c Choice) Decode(v any) error {
	raw, ok := c.Options[c.Use]
	if !ok {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("options of %q: %w", c.Use, err)
	}
	return nil
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{Env: "development", Listen: "127.0.0.1:8080"}
	cfg.Store.Use = "sqlite"
	if path == "" {
		cfg.Auth.Dev = true
		cfg.Worker.Inline = true
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(os.ExpandEnv(string(b))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if cfg.Env != "development" && cfg.Env != "production" {
		return nil, fmt.Errorf("config: env must be development or production")
	}
	if cfg.Env == "production" && cfg.Auth.Dev {
		return nil, fmt.Errorf("config: dev auth is not allowed in production")
	}
	if !cfg.Auth.Dev && len(cfg.Auth.Keys) == 0 {
		return nil, fmt.Errorf("config: configure auth.keys (or auth.dev for local development)")
	}
	return cfg, nil
}
