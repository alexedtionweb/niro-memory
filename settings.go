package niromem

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/alexedtionweb/niro-memory/knowledge"
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
	// Knowledge narrows server bounds. Mode/provider changes beyond startup
	// permissions require explicitly registered KnowledgePlanners.
	Knowledge *KnowledgePolicy `json:"knowledge,omitempty"`
}

type KnowledgePolicy struct {
	Extraction *knowledge.ExtractionProfile `json:"extraction,omitempty"`
	Mode       string                       `json:"mode,omitempty"`
	// Planner selects a server-registered runtime profile. Without runtime
	// profiles, Mode can only narrow the startup permissions.
	Planner       string `json:"planner,omitempty"`
	MaxCandidates int    `json:"max_candidates,omitempty"`
	MaxGraphHops  int    `json:"max_graph_hops,omitempty"`
}

func (e *Engine) knowledgeOptions(ctx context.Context, tenant string) (KnowledgeOptions, error) {
	o := e.o.Knowledge
	s, err := e.Settings(ctx, tenant)
	if err != nil {
		return o, err
	}
	if p := s.Knowledge; p != nil {
		if err := e.validateKnowledgePolicy(p); err != nil {
			return o, err
		}
		if p.Planner != "" {
			o.Planner = e.o.KnowledgePlanners[p.Planner]
		}
		if p.Extraction != nil {
			o.Extraction = p.Extraction
		}
		if p.Mode != "" {
			o.Mode = p.Mode
		}
		if p.MaxCandidates > 0 && p.MaxCandidates < o.MaxCandidates {
			o.MaxCandidates = p.MaxCandidates
		}
		if p.MaxGraphHops > 0 && p.MaxGraphHops < o.MaxGraphHops {
			o.MaxGraphHops = p.MaxGraphHops
		}
	}
	return o, nil
}

func (e *Engine) validateKnowledgePolicy(p *KnowledgePolicy) error {
	if p == nil {
		return nil
	}
	if p.Extraction != nil {
		if err := p.Extraction.Validate(); err != nil {
			return err
		}
	}
	o := e.o.Knowledge
	if p.Mode != "" && (knowledgeModeRank(p.Mode) < 0 ||
		len(e.o.KnowledgePlanners) == 0 && knowledgeModeRank(p.Mode) > knowledgeModeRank(o.Mode)) ||
		p.MaxCandidates < 0 || p.MaxCandidates > o.MaxCandidates ||
		p.MaxGraphHops < 0 || p.MaxGraphHops > o.MaxGraphHops {
		return fmt.Errorf("%w: tenant knowledge policy exceeds server permissions or bounds", memory.ErrInvalid)
	}
	if p.Planner != "" && e.o.KnowledgePlanners[p.Planner] == nil {
		return fmt.Errorf("%w: unknown or unavailable knowledge planner profile %q", memory.ErrInvalid, p.Planner)
	}
	return nil
}

// KnowledgeConfiguration exposes no credentials or provider endpoints.
type KnowledgeConfiguration struct {
	Extraction           knowledge.ExtractionProfile `json:"extraction"`
	ExtractionSchema     json.RawMessage             `json:"extraction_schema"`
	Mode                 string                      `json:"mode"`
	Provider             string                      `json:"provider"`
	Model                string                      `json:"model,omitempty"`
	MaxCandidates        int                         `json:"max_candidates"`
	MaxGraphHops         int                         `json:"max_graph_hops"`
	TimeoutMS            int64                       `json:"timeout_ms"`
	Fallback             bool                        `json:"fallback"`
	Consolidation        bool                        `json:"consolidation"`
	Admission            bool                        `json:"admission"`
	BackgroundEnrichment bool                        `json:"background_enrichment"`
	ExtractionMode       string                      `json:"extraction_mode,omitempty"`
}

type KnowledgeConfigurationReport struct {
	Server    KnowledgeConfiguration            `json:"server"`
	Effective KnowledgeConfiguration            `json:"effective"`
	Policy    *KnowledgePolicy                  `json:"policy"`
	Runtime   bool                              `json:"runtime"`
	Profiles  map[string]KnowledgeConfiguration `json:"profiles"`
}

func describeKnowledge(o KnowledgeOptions) (KnowledgeConfiguration, error) {
	provider, model := fmt.Sprintf("%T", o.Planner), ""
	if o.Planner == nil {
		provider = "deterministic"
	} else if _, ok := o.Planner.(knowledge.Deterministic); ok {
		provider = "deterministic"
	} else if d, ok := o.Planner.(interface{ KnowledgeProvider() (string, string) }); ok {
		provider, model = d.KnowledgeProvider()
	}
	profile, err := knowledge.EffectiveExtractionProfile(o.Extraction)
	if err != nil {
		return KnowledgeConfiguration{}, err
	}
	schema, err := knowledge.ExtractionJSONSchema(&profile)
	if err != nil {
		return KnowledgeConfiguration{}, err
	}
	extractionMode := ""
	if d, ok := o.Planner.(interface{ KnowledgeExtractionMode() string }); ok {
		extractionMode = d.KnowledgeExtractionMode()
	}
	return KnowledgeConfiguration{Mode: o.Mode, Provider: provider, Model: model, MaxCandidates: o.MaxCandidates, MaxGraphHops: o.MaxGraphHops,
		TimeoutMS: o.Timeout.Milliseconds(), Fallback: o.Fallback, Consolidation: o.Consolidation, Admission: o.Admission != nil, BackgroundEnrichment: o.BackgroundEnrichment,
		Extraction: profile, ExtractionSchema: schema, ExtractionMode: extractionMode}, nil
}

// KnowledgeConfiguration returns effective tenant permissions and registered
// runtime profiles. Runtime choices remain durable settings, not node state.
func (e *Engine) KnowledgeConfiguration(ctx context.Context, tenant string) (KnowledgeConfigurationReport, error) {
	effective, err := e.knowledgeOptions(ctx, tenant)
	if err != nil {
		return KnowledgeConfigurationReport{}, err
	}
	settings, err := e.Settings(ctx, tenant)
	if err != nil {
		return KnowledgeConfigurationReport{}, err
	}
	profiles := map[string]KnowledgeConfiguration{}
	for name, planner := range e.o.KnowledgePlanners {
		o := e.o.Knowledge
		o.Planner = planner
		description, err := describeKnowledge(o)
		if err != nil {
			return KnowledgeConfigurationReport{}, err
		}
		profiles[name] = description
	}
	startup, err := describeKnowledge(e.o.Knowledge)
	if err != nil {
		return KnowledgeConfigurationReport{}, err
	}
	description, err := describeKnowledge(effective)
	if err != nil {
		return KnowledgeConfigurationReport{}, err
	}
	return KnowledgeConfigurationReport{startup, description, settings.Knowledge,
		len(profiles) != 0, profiles}, nil
}

func knowledgeModeRank(mode string) int {
	switch mode {
	case "disabled":
		return 0
	case "shadow":
		return 1
	case "enforced":
		return 2
	default:
		return -1
	}
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
		if p.RelationshipHops < 0 || p.RelationshipHops > 4 {
			return fmt.Errorf("%w: relationship_hops must be 0..4", memory.ErrInvalid)
		}
		if p.Name == "" {
			p.Name = name
			s.Policies[name] = p
		}
	}
	if err := e.validateKnowledgePolicy(s.Knowledge); err != nil {
		return err
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
