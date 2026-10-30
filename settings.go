package niromem

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/alexedtionweb/niro-memory/memory"
	"github.com/alexedtionweb/niro-memory/rules"
	"github.com/alexedtionweb/niro-memory/search"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/vector"
)

// Settings is the per-tenant configuration document, stored in the store
// so every stateless node sees the same configuration.
type Settings struct {
	// Rules overrides the engine's global rules for this tenant.
	Rules json.RawMessage `json:"rules,omitempty"`
	// VectorModels declares the embedding spaces this tenant writes. The
	// configured embedder's model is implicitly declared.
	VectorModels []vector.Model `json:"vector_models,omitempty"`
	// Bindings restrict what agents may read (their "loadout").
	Bindings []Binding `json:"bindings,omitempty"`
	// Policies adds or overrides retrieval policies by name.
	Policies map[string]search.Policy `json:"policies,omitempty"`
	// DefaultPolicy is used when a request names none (default "default").
	DefaultPolicy string `json:"default_policy,omitempty"`
}

// Binding limits what one agent may read, on top of memory visibility.
type Binding struct {
	Agent string `json:"agent"`
	// Users the agent may read memories of; empty means all users.
	Users []string `json:"users,omitempty"`
	// ReadAgents are other agents whose memories it may read. Its own and
	// agent-less memories are always readable.
	ReadAgents []string `json:"read_agents,omitempty"`
}

// Settings returns the tenant's settings (cached for SettingsTTL).
func (e *Engine) Settings(ctx context.Context, tenant string) (Settings, error) {
	key := "settings:" + tenant
	var raw []byte
	if b, ok := e.o.Cache.Get(ctx, key); ok {
		raw = b
	} else {
		var err error
		if raw, err = e.o.Store.Settings(ctx, tenant); err != nil {
			return Settings{}, err
		}
		e.o.Cache.Set(ctx, key, raw, e.o.SettingsTTL)
	}
	var s Settings
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("settings of %s: %w", tenant, err)
		}
	}
	return s, nil
}

// PutSettings validates and replaces the tenant's settings.
func (e *Engine) PutSettings(ctx context.Context, tenant string, s Settings) error {
	if tenant == "" {
		return fmt.Errorf("%w: tenant is required", memory.ErrInvalid)
	}
	if len(s.Rules) > 0 && string(s.Rules) != "null" {
		if _, err := rules.Parse(s.Rules); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	active := 0
	for _, m := range s.VectorModels {
		if m.ID == "" || m.Dims <= 0 || m.Dims > 16384 || seen[m.ID] {
			return fmt.Errorf("%w: vector model %q needs a unique id and 0 < dims <= 16384", memory.ErrInvalid, m.ID)
		}
		seen[m.ID] = true
		if m.Active {
			active++
		}
	}
	if active > 1 {
		return fmt.Errorf("%w: at most one active vector model", memory.ErrInvalid)
	}
	for name, p := range s.Policies {
		if p.Name == "" {
			p.Name = name
			s.Policies[name] = p
		}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if _, err := e.o.Store.Write(ctx, &store.Batch{Tenant: tenant, Settings: raw}); err != nil {
		return err
	}
	e.o.Cache.Set(ctx, "settings:"+tenant, raw, e.o.SettingsTTL)
	return nil
}

// rulesFor returns the tenant's rules override, or the global rules.
func (e *Engine) rulesFor(ctx context.Context, tenant string) (*rules.Set, error) {
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if len(s.Rules) == 0 || string(s.Rules) == "null" {
		return e.o.Rules, nil
	}
	key := string(s.Rules)
	e.rulesMu.Lock()
	defer e.rulesMu.Unlock()
	if rs, ok := e.rulesBy[key]; ok {
		return rs, nil
	}
	rs, err := rules.Parse(s.Rules)
	if err != nil {
		return nil, err
	}
	if len(e.rulesBy) > 256 { // bounded: rule documents change rarely
		e.rulesBy = map[string]*rules.Set{}
	}
	e.rulesBy[key] = rs
	return rs, nil
}

// Rules returns the effective rules of a tenant (nil if none).
func (e *Engine) Rules(ctx context.Context, tenant string) (*rules.Set, error) {
	return e.rulesFor(ctx, tenant)
}

// models returns the declared vector models of a tenant, including the
// embedder's model, and the active one used for query text.
func (e *Engine) models(s Settings) (map[string]vector.Model, string) {
	out := map[string]vector.Model{}
	active := ""
	if e.o.Embedder != nil {
		m := e.o.Embedder.Model()
		out[m.ID] = m
		active = m.ID
	}
	for _, m := range s.VectorModels {
		out[m.ID] = m
		if m.Active {
			active = m.ID
		}
	}
	return out, active
}

// policy resolves a policy name for a tenant.
func (e *Engine) policy(s Settings, name string) (search.Policy, error) {
	if name == "" {
		name = s.DefaultPolicy
	}
	if name == "" {
		name = "default"
	}
	if p, ok := s.Policies[name]; ok {
		return p, nil
	}
	if p, ok := search.Default[name]; ok {
		return p, nil
	}
	return search.Policy{}, fmt.Errorf("%w: unknown policy %q", memory.ErrInvalid, name)
}

// Policies lists the policy names available to a tenant.
func (e *Engine) Policies(ctx context.Context, tenant string) ([]string, error) {
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var out []string
	for n := range search.Default {
		out = append(out, n)
	}
	for n := range s.Policies {
		if _, dup := search.Default[n]; !dup {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// bindingFilter narrows reads of an agent to its binding, if it has one.
func bindingFilter(s Settings, as memory.Principal) memory.Filter {
	if as.Admin || as.Agent == "" {
		return memory.Filter{}
	}
	for _, b := range s.Bindings {
		if b.Agent != as.Agent {
			continue
		}
		agents := []any{"", as.Agent}
		for _, a := range b.ReadAgents {
			agents = append(agents, a)
		}
		f := memory.And(memory.Cmp("agent_id", memory.OpIn, agents))
		if len(b.Users) > 0 {
			users := []any{}
			for _, u := range b.Users {
				users = append(users, u)
			}
			f.And = append(f.And, memory.Cmp("user_id", memory.OpIn, users))
		}
		return f
	}
	return memory.Filter{}
}
