// Package opencode reads and updates the parts of an OpenCode configuration
// that control which OpenRouter models are available.
//
// OpenCode does not have a model-level allowlist. The closest equivalent is a
// dedicated provider that uses "canonical": "openrouter" and lists exactly the
// models that should be selectable, while a policy denies the raw "openrouter"
// provider. This package maintains that layout and leaves unrelated settings
// alone.
package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/tailscale/hujson"
)

const (
	// RawProvider is OpenCode's built-in OpenRouter provider.
	RawProvider = "openrouter"
	// CuratedProvider is the allowlist provider managed by orb.
	CuratedProvider = "openrouter-curated"
	// SchemaURL is included in newly created configuration files.
	SchemaURL = "https://opencode.ai/config.json"

	// providerPolicy is the policy action that grants or denies a provider.
	providerPolicy = "provider.use"
)

// Routing is the OpenRouter upstream routing for a single model.
type Routing struct {
	// Order is the list of upstream provider slugs passed to OpenRouter.
	Order []string
	// AllowFallbacks controls OpenRouter's allow_fallbacks option.
	AllowFallbacks bool
	// Configured is true when the model has explicit provider routing.
	Configured bool
}

// Config is an OpenCode configuration document that can be edited in memory
// and written back atomically.
type Config struct {
	Path string

	raw      map[string]any
	original map[string]any
	doc      hujson.Value

	// models is the curated provider's model map. It is only merged into raw
	// when the configuration is saved.
	models map[string]any
	// rawModels preserves entries from the built-in openrouter provider so
	// that routing settings survive when a model is allowed again.
	rawModels map[string]any
	// removed keeps entries from models that were toggled off during this
	// session, so toggling them back on restores their routing.
	removed map[string]any

	isNew bool
	dirty bool
}

// ResolvePath returns the OpenCode configuration file to edit. An explicit
// path wins; otherwise the global opencode.json (or opencode.jsonc) is used.
// ResolvePath returns the OpenCode configuration file to edit. An explicit
// path wins; otherwise the global opencode.json (or opencode.jsonc) is used.
func ResolvePath(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	base := filepath.Join(dir, "opencode")
	for _, name := range []string{"opencode.json", "opencode.jsonc"} {
		p := filepath.Join(base, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return filepath.Join(base, "opencode.json"), nil
}

// Load reads an OpenCode configuration. A missing file produces an empty
// configuration that will be created on the first save.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	isNew := false
	if errors.Is(err, fs.ErrNotExist) {
		isNew = true
		data = []byte("{}\n")
	} else if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		isNew = true
		data = []byte("{}\n")
	}

	doc, err := hujson.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	std := doc.Clone()
	std.Standardize()
	var raw map[string]any
	if err := json.Unmarshal(std.Pack(), &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}

	c := &Config{
		Path:      path,
		raw:       raw,
		original:  deepCopy(raw).(map[string]any),
		doc:       doc,
		isNew:     isNew,
		rawModels: rawModelMap(raw),
		removed:   map[string]any{},
	}
	c.models = c.seedModels()
	return c, nil
}

// Dirty reports whether the in-memory configuration differs from what was
// loaded or last saved.
func (c *Config) Dirty() bool { return c.dirty }

// Allowed reports whether modelID is in the curated allowlist.
func (c *Config) Allowed(modelID string) bool {
	entry, ok := c.models[modelID].(map[string]any)
	if !ok {
		return false
	}
	if disabled, ok := entry["disabled"].(bool); ok && disabled {
		return false
	}
	return true
}

// AllowedIDs returns a set of allowed model IDs.
func (c *Config) AllowedIDs() map[string]bool {
	out := make(map[string]bool, len(c.models))
	for id := range c.models {
		if c.Allowed(id) {
			out[id] = true
		}
	}
	return out
}

// SetAllowed adds or removes a model from the curated allowlist.
func (c *Config) SetAllowed(modelID string, allowed bool) {
	if allowed {
		if c.Allowed(modelID) {
			return
		}
		entry := c.entry(modelID)
		delete(entry, "disabled")
		c.models[modelID] = entry
		c.dirty = true
		return
	}

	if _, exists := c.models[modelID]; !exists {
		return
	}
	c.removed[modelID] = c.models[modelID]
	delete(c.models, modelID)
	c.dirty = true
}

// Routing returns the explicit routing settings for a model.
func (c *Config) Routing(modelID string) Routing {
	r := Routing{AllowFallbacks: true}
	entry, ok := c.models[modelID].(map[string]any)
	if !ok {
		return r
	}
	body, _ := entry["body"].(map[string]any)
	provider, _ := body["provider"].(map[string]any)
	if provider == nil {
		return r
	}
	if v, ok := provider["allow_fallbacks"].(bool); ok {
		r.AllowFallbacks = v
		r.Configured = true
	}
	if values, ok := provider["order"].([]any); ok {
		for _, value := range values {
			if s, ok := value.(string); ok && s != "" {
				r.Order = append(r.Order, s)
			}
		}
		if len(r.Order) > 0 {
			r.Configured = true
		}
	}
	return r
}

// SetRouting stores explicit routing settings for a model and allows it.
func (c *Config) SetRouting(modelID string, r Routing) {
	if !c.Allowed(modelID) {
		c.SetAllowed(modelID, true)
	}
	entry := c.entry(modelID)
	body := ensureMap(entry, "body")
	provider := ensureMap(body, "provider")

	if len(r.Order) == 0 {
		delete(provider, "order")
	} else {
		order := make([]any, len(r.Order))
		for i, slug := range r.Order {
			order[i] = slug
		}
		provider["order"] = order
	}
	provider["allow_fallbacks"] = r.AllowFallbacks

	if len(provider) == 0 {
		delete(body, "provider")
	}
	if len(body) == 0 {
		delete(entry, "body")
	}
	c.dirty = true
}

// Agents returns the sorted names of the agents defined in the configuration.
func (c *Config) Agents() []string {
	agents, _ := c.raw["agents"].(map[string]any)
	names := make([]string, 0, len(agents))
	for name, value := range agents {
		if _, ok := value.(map[string]any); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// AgentModel returns the full model reference configured for an agent.
func (c *Config) AgentModel(agent string) (string, bool) {
	entry, ok := c.agentEntry(agent)
	if !ok {
		return "", false
	}
	ref, ok := parseModelRef(entry["model"])
	if !ok {
		return "", false
	}
	value, _ := ref.value().(string)
	return value, true
}

// AgentUsesModel reports whether an agent is configured to use modelID,
// referenced through either the raw or the curated provider.
func (c *Config) AgentUsesModel(agent, modelID string) bool {
	entry, ok := c.agentEntry(agent)
	if !ok {
		return false
	}
	ref, ok := parseModelRef(entry["model"])
	if !ok || (ref.Provider != RawProvider && ref.Provider != CuratedProvider) {
		return false
	}
	return ref.Model == modelID
}

// AgentsUsing returns the sorted names of agents configured to use modelID.
func (c *Config) AgentsUsing(modelID string) []string {
	var names []string
	for _, name := range c.Agents() {
		if c.AgentUsesModel(name, modelID) {
			names = append(names, name)
		}
	}
	return names
}

// SetAgentModel points an agent's model at the curated provider and makes
// sure the model is allowed. Other agent settings are left untouched.
func (c *Config) SetAgentModel(agent, modelID string) {
	agent = strings.TrimSpace(agent)
	if agent == "" || modelID == "" {
		return
	}
	if !c.Allowed(modelID) {
		c.SetAllowed(modelID, true)
	}

	agents := ensureMap(c.raw, "agents")
	entry := ensureMap(agents, agent)
	entry["model"] = CuratedProvider + "/" + modelID
	c.dirty = true
}

// ClearAgentModel removes an agent's model assignment while keeping its other
// settings. An agent with no remaining settings is removed entirely.
func (c *Config) ClearAgentModel(agent string) {
	agents, _ := c.raw["agents"].(map[string]any)
	entry, ok := agents[agent].(map[string]any)
	if !ok {
		return
	}
	if _, ok := entry["model"]; !ok {
		return
	}

	delete(entry, "model")
	if len(entry) == 0 {
		delete(agents, agent)
		if len(agents) == 0 {
			delete(c.raw, "agents")
		}
	}
	c.dirty = true
}

// agentEntry returns an agent's configuration object.
func (c *Config) agentEntry(agent string) (map[string]any, bool) {
	agents, _ := c.raw["agents"].(map[string]any)
	entry, ok := agents[agent].(map[string]any)
	return entry, ok
}

// Save writes the curated provider, the provider policies, and any rewritten
// model references back to disk. Comments and unrelated settings in JSONC
// files are preserved.
func (c *Config) Save() error {
	if c.isNew {
		if _, ok := c.raw["$schema"]; !ok {
			c.raw["$schema"] = SchemaURL
		}
	}

	c.ensureCuratedProvider()
	c.ensurePolicies(RawProvider, CuratedProvider)
	c.rewriteModelReferences()

	patch, err := c.buildPatch()
	if err != nil {
		return err
	}
	if len(patch) == 0 {
		c.dirty = false
		return nil
	}

	doc := c.doc.Clone()
	if err := doc.Patch(patch); err != nil {
		return fmt.Errorf("update %s: %w", c.Path, err)
	}

	var out []byte
	if c.doc.IsStandard() {
		// A plain JSON file has no comments to preserve, so emit clean,
		// consistently indented JSON instead of HuJSON's compact insertions.
		out, err = marshalIndent(c.raw)
		if err != nil {
			return fmt.Errorf("encode %s: %w", c.Path, err)
		}
	} else {
		doc.Format()
		out = doc.Pack()
	}
	if len(out) == 0 || out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}

	mode := fs.FileMode(0o600)
	if info, err := os.Stat(c.Path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(c.Path, out, mode); err != nil {
		return err
	}

	c.doc = doc
	c.original = deepCopy(c.raw).(map[string]any)
	c.isNew = false
	c.dirty = false
	return nil
}

// Warnings describes model references that still point at the raw provider or
// at a model that is not in the allowlist. References in external agent and
// command files cannot be inspected and are not included.
func (c *Config) Warnings() []string {
	var warnings []string
	if len(c.AllowedIDs()) == 0 {
		warnings = append(warnings, "the curated OpenRouter allowlist is empty")
	}
	check := func(where string, value any) {
		ref, ok := parseModelRef(value)
		if !ok {
			return
		}
		switch {
		case ref.Provider == RawProvider:
			warnings = append(warnings, fmt.Sprintf("%s still uses %s/%s", where, ref.Provider, ref.Model))
		case ref.Provider == CuratedProvider && !c.Allowed(ref.Model):
			warnings = append(warnings, fmt.Sprintf("%s uses a model that is not allowed: %s", where, ref.Model))
		}
	}

	if value, ok := c.raw["model"]; ok {
		check("default model", value)
	}
	for _, section := range []string{"agents", "commands"} {
		entries, _ := c.raw[section].(map[string]any)
		for name, value := range entries {
			entry, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if model, ok := entry["model"]; ok {
				check(section+"/"+name, model)
			}
		}
	}
	return warnings
}

// entry returns the model's stored entry, preferring the curated list and
// falling back to entries removed this session or left on the raw provider,
// so re-allowing a model restores its previous settings.
func (c *Config) entry(modelID string) map[string]any {
	if entry, ok := c.models[modelID].(map[string]any); ok {
		return entry
	}
	if entry, ok := c.removed[modelID].(map[string]any); ok {
		return deepCopy(entry).(map[string]any)
	}
	if entry, ok := c.rawModels[modelID].(map[string]any); ok {
		return deepCopy(entry).(map[string]any)
	}
	return map[string]any{}
}

// seedModels initializes the curated model map at load time, either from an
// existing curated provider or by migrating overrides from the raw one.
func (c *Config) seedModels() map[string]any {
	providers, _ := c.raw["providers"].(map[string]any)

	if provider, ok := providers[CuratedProvider].(map[string]any); ok {
		models, _ := provider["models"].(map[string]any)
		return allowedEntries(models)
	}

	// Migrate the model overrides that were attached to the raw provider.
	return allowedEntries(c.rawModels)
}

// ensureCuratedProvider creates the curated provider if missing, copying
// non-model settings from the raw provider, and syncs the model map into it.
func (c *Config) ensureCuratedProvider() {
	providers := ensureMap(c.raw, "providers")

	provider, ok := providers[CuratedProvider].(map[string]any)
	if !ok {
		provider = map[string]any{}
		if raw, ok := providers[RawProvider].(map[string]any); ok {
			for key, value := range raw {
				if key == "models" || key == "canonical" {
					continue
				}
				provider[key] = deepCopy(value)
			}
		}
		providers[CuratedProvider] = provider
	}
	provider["canonical"] = RawProvider
	if _, ok := provider["env"]; !ok {
		// canonical inherits the catalog, not the credential lookup. Without
		// this, an OPENROUTER_API_KEY only authenticates the raw provider.
		provider["env"] = []any{"OPENROUTER_API_KEY"}
	}
	provider["models"] = c.models
}

// ensurePolicies adds (or refreshes) the deny/allow policy pair that hides
// the raw provider and exposes the curated one, leaving other policies in
// their original relative order.
func (c *Config) ensurePolicies(denyProvider, allowProvider string) {
	existing, _ := c.raw["policies"].([]any)
	policies := make([]any, 0, len(existing)+2)
	for _, value := range existing {
		statement, ok := value.(map[string]any)
		if ok && statement["action"] == providerPolicy &&
			(statement["resource"] == denyProvider || statement["resource"] == allowProvider) {
			// Drop old statements for the two managed providers; they are
			// appended below so their relative order is deterministic.
			continue
		}
		policies = append(policies, value)
	}
	policies = append(policies,
		map[string]any{"action": providerPolicy, "resource": denyProvider, "effect": "deny"},
		map[string]any{"action": providerPolicy, "resource": allowProvider, "effect": "allow"},
	)
	c.raw["policies"] = policies
}

// rewriteModelReferences repoints "openrouter/<model>" references for
// allowed models at the curated provider so existing settings keep working.
func (c *Config) rewriteModelReferences() {
	rewrite := func(value any) any {
		ref, ok := parseModelRef(value)
		if !ok || ref.Provider != RawProvider || !c.Allowed(ref.Model) {
			return value
		}
		ref.Provider = CuratedProvider
		return ref.value()
	}

	if value, ok := c.raw["model"]; ok {
		c.raw["model"] = rewrite(value)
	}
	for _, section := range []string{"agents", "commands"} {
		entries, _ := c.raw[section].(map[string]any)
		for _, value := range entries {
			entry, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if model, ok := entry["model"]; ok {
				entry["model"] = rewrite(model)
			}
		}
	}
}

// patchOperation is one RFC 6902 JSON Patch operation used to update the
// HuJSON document in place.
type patchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// buildPatch serializes the differences between original and current state
// into a JSON Patch covering the schema, providers, policies and model
// references.
func (c *Config) buildPatch() ([]byte, error) {
	var operations []patchOperation
	add := func(path string, before, after any, hadBefore bool) {
		if hadBefore && reflect.DeepEqual(before, after) {
			return
		}
		op := "add"
		if hadBefore {
			op = "replace"
		}
		operations = append(operations, patchOperation{Op: op, Path: path, Value: after})
	}

	if before, had := c.original["$schema"]; had {
		add("/$schema", before, c.raw["$schema"], true)
	} else if after, has := c.raw["$schema"]; has {
		add("/$schema", nil, after, false)
	}

	beforeProviders, hadProviders := c.original["providers"].(map[string]any)
	afterProviders, _ := c.raw["providers"].(map[string]any)
	if !hadProviders {
		if after, has := c.raw["providers"]; has {
			add("/providers", nil, after, false)
		}
	} else {
		beforeCurated, hadCurated := beforeProviders[CuratedProvider]
		afterCurated, hasCurated := afterProviders[CuratedProvider]
		switch {
		case hasCurated:
			add("/providers/"+escapePointer(CuratedProvider), beforeCurated, afterCurated, hadCurated)
		case hadCurated:
			operations = append(operations, patchOperation{Op: "remove", Path: "/providers/" + escapePointer(CuratedProvider)})
		}
	}

	if before, had := c.original["policies"]; had {
		add("/policies", before, c.raw["policies"], true)
	} else if after, has := c.raw["policies"]; has {
		add("/policies", nil, after, false)
	}

	if before, had := c.original["model"]; had {
		add("/model", before, c.raw["model"], true)
	} else if after, has := c.raw["model"]; has {
		add("/model", nil, after, false)
	}

	for _, section := range []string{"agents", "commands"} {
		beforeSection, hadSection := c.original[section].(map[string]any)
		afterSection, hasSection := c.raw[section].(map[string]any)
		switch {
		case !hadSection && hasSection:
			// The section is new, so add it whole; JSON Patch cannot add a
			// nested value when its parent does not exist.
			add("/"+section, nil, c.raw[section], false)
			continue
		case hadSection && !hasSection:
			operations = append(operations, patchOperation{Op: "remove", Path: "/" + section})
			continue
		case !hadSection && !hasSection:
			continue
		}

		for name, afterValue := range afterSection {
			afterEntry, _ := afterValue.(map[string]any)
			beforeEntry, hadEntry := beforeSection[name].(map[string]any)
			if !hadEntry {
				// Add the whole agent or command so its parent exists.
				add("/"+section+"/"+escapePointer(name), nil, afterValue, false)
				continue
			}
			afterModel, afterHas := afterEntry["model"]
			beforeModel, beforeHas := beforeEntry["model"]
			if afterHas == beforeHas && reflect.DeepEqual(afterModel, beforeModel) {
				continue
			}
			path := "/" + section + "/" + escapePointer(name) + "/model"
			if afterHas {
				add(path, beforeModel, afterModel, beforeHas)
			} else if beforeHas {
				operations = append(operations, patchOperation{Op: "remove", Path: path})
			}
		}
		for name := range beforeSection {
			if _, ok := afterSection[name]; !ok {
				operations = append(operations, patchOperation{Op: "remove", Path: "/" + section + "/" + escapePointer(name)})
			}
		}
	}

	if len(operations) == 0 {
		return nil, nil
	}
	return json.Marshal(operations)
}

// escapePointer encodes a key as a JSON Pointer token (RFC 6901).
func escapePointer(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

// modelRef is a parsed OpenCode model reference such as
// "provider/model" or "provider/model#variant".
type modelRef struct {
	Provider string
	Model    string
	Variant  string
}

// value renders the reference back into OpenCode's string form.
func (r modelRef) value() any {
	if r.Variant == "" {
		return r.Provider + "/" + r.Model
	}
	return r.Provider + "/" + r.Model + "#" + r.Variant
}

// parseModelRef extracts a model reference from either the string form or
// OpenCode's {providerID, model, variant} object form.
func parseModelRef(value any) (modelRef, bool) {
	switch v := value.(type) {
	case string:
		ref, variant, _ := strings.Cut(v, "#")
		provider, model, ok := strings.Cut(ref, "/")
		if !ok || provider == "" || model == "" {
			return modelRef{}, false
		}
		return modelRef{Provider: provider, Model: model, Variant: variant}, true
	case map[string]any:
		provider, _ := v["providerID"].(string)
		model, _ := v["model"].(string)
		variant, _ := v["variant"].(string)
		if provider == "" || model == "" {
			return modelRef{}, false
		}
		return modelRef{Provider: provider, Model: model, Variant: variant}, true
	default:
		return modelRef{}, false
	}
}

// rawModelMap returns the model overrides attached to the built-in
// OpenRouter provider, if any.
func rawModelMap(raw map[string]any) map[string]any {
	providers, _ := raw["providers"].(map[string]any)
	provider, _ := providers[RawProvider].(map[string]any)
	models, _ := provider["models"].(map[string]any)
	return models
}

// allowedEntries copies a model map, dropping entries explicitly disabled
// with "disabled": true.
func allowedEntries(models map[string]any) map[string]any {
	out := make(map[string]any, len(models))
	for id, value := range models {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if disabled, ok := entry["disabled"].(bool); ok && disabled {
			continue
		}
		out[id] = deepCopy(value)
	}
	return out
}

// ensureMap returns the object stored at key, creating an empty one if the
// key is missing or holds something else.
func ensureMap(parent map[string]any, key string) map[string]any {
	if value, ok := parent[key].(map[string]any); ok {
		return value
	}
	value := map[string]any{}
	parent[key] = value
	return value
}

// deepCopy clones a JSON-shaped value via a marshal/unmarshal round trip,
// so edits to the copy never alias the original document.
func deepCopy(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return value
	}
	return out
}

// marshalIndent encodes as two-space indented JSON without HTML escaping,
// so model IDs containing & or < survive intact.
func marshalIndent(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes data to path via a temp file and rename, so a
// crash mid-write can never leave a truncated config behind.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".orb-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmp := file.Name()
	defer func() { _ = os.Remove(tmp) }()

	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return fmt.Errorf("set config mode: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
