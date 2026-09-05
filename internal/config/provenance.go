package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Origin says where a resolved value came from. Recorded by LoadFromPath as
// each step of the pipeline runs — a record of what happened, not a
// re-derivation afterwards (resolved-config provenance design §3).
type Origin string

const (
	// OriginDefault — DefaultConfig() set a non-zero value and nothing changed it.
	OriginDefault Origin = "default"
	// OriginFile — the key was present in the config file (presence, not value:
	// a value written explicitly is `file` even when it equals the default).
	OriginFile Origin = "file"
	// OriginAlias — copied from the artifacts.* alias block; Source is the key copied from.
	OriginAlias Origin = "alias"
	// OriginPlaceholder — a ${VAR} in the file was expanded; Source names the variables.
	OriginPlaceholder Origin = "placeholder"
	// OriginEnv — a VORNIK_* variable overrode it; Source is the variable.
	OriginEnv Origin = "env"
	// OriginEnvInvalid — a VORNIK_* variable was set and did not parse; the
	// prior value stands. Source is the variable and the parse error.
	OriginEnvInvalid Origin = "env_invalid"
	// OriginDerived — a post-parse default filled it (applyAuthDefaults, Composer.applyDefaults).
	OriginDerived Origin = "derived"
	// OriginSecretFile — read from a *_file path; Source is the file's basename.
	OriginSecretFile Origin = "secret_file"
	// OriginUnset — the zero value; nothing set it. The origin that says a key
	// the operator believes is configured was never bound.
	OriginUnset Origin = "unset"
)

// ValueOrigin is one leaf's provenance.
type ValueOrigin struct {
	Origin Origin `json:"origin"`
	Source string `json:"source,omitempty"`
}

// Provenance is the per-key origin map for one load.
type Provenance struct {
	Path     string                 `json:"config_path"`
	LoadedAt time.Time              `json:"loaded_at"`
	Values   map[string]ValueOrigin `json:"values"`
}

func newProvenance(path string) *Provenance {
	return &Provenance{Path: path, LoadedAt: time.Now().UTC(), Values: map[string]ValueOrigin{}}
}

func (p *Provenance) set(key string, o Origin, source string) {
	p.Values[key] = ValueOrigin{Origin: o, Source: source}
}

// Keys returns the recorded keys, sorted.
func (p *Provenance) Keys() []string {
	out := make([]string, 0, len(p.Values))
	for k := range p.Values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// leafSnapshot renders every walked leaf to a comparable string, so two
// pipeline stages can be diffed key by key without knowing which fields a
// stage touches.
func leafSnapshot(cfg *Config) map[string]string {
	out := map[string]string{}
	WalkLeaves(reflect.ValueOf(cfg).Elem(), func(key string, _ reflect.StructField, v reflect.Value) {
		out[key] = renderLeaf(v)
	})
	return out
}

func renderLeaf(v reflect.Value) string {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "<nil>"
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return "<nil>"
	}
	return fmt.Sprintf("%v", v.Interface())
}

func leafIsZero(v reflect.Value) bool {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return true
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	}
	return v.IsZero()
}

// presentKeys walks the raw document and returns every dotted key present:
// a mapping is recursed into, a scalar or sequence is a present leaf, and an
// EMPTY mapping is present as a leaf too (an operator who wrote `x: {}`
// wrote something).
func presentKeys(raw []byte, isJSON bool) (map[string]bool, error) {
	out := map[string]bool{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return out, nil
	}
	if isJSON {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		presentJSONKeys("", doc, out)
		return out, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	presentYAMLKeys("", &doc, out)
	return out, nil
}

func presentJSONKeys(prefix string, m map[string]any, out map[string]bool) {
	for k, v := range m {
		key := joinKey(prefix, k)
		if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
			presentJSONKeys(key, sub, out)
			continue
		}
		out[key] = true
	}
}

func presentYAMLKeys(prefix string, n *yaml.Node, out map[string]bool) {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		presentYAMLKeys(prefix, n.Content[0], out)
		return
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		key := joinKey(prefix, k.Value)
		if v.Kind == yaml.MappingNode && len(v.Content) > 0 {
			presentYAMLKeys(key, v, out)
			continue
		}
		out[key] = true
	}
}

func joinKey(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

var rePlaceholderVar = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// placeholderVars names the ${VAR}/$VAR references in a rendered leaf.
func placeholderVars(rendered string) string {
	var names []string
	seen := map[string]bool{}
	for _, m := range rePlaceholderVar.FindAllStringSubmatch(rendered, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, "${"+m[1]+"}")
		}
	}
	return strings.Join(names, ",")
}

// aliasMirror is one artifacts.* -> storage.* copy, with the condition the
// hand-written block applied. Recorded as `alias` when it fires.
type aliasMirror struct {
	to, from string
	when     func(c *Config) bool
	copy     func(c *Config)
}

var aliasMirrors = []aliasMirror{
	// Accept both the current `storage.artifacts_path` shape and the guide's
	// `artifacts.storagePath` shape.
	{to: "storage.artifacts_path", from: "artifacts.artifacts_path",
		when: func(c *Config) bool { return c.Artifacts.ArtifactsPath != "" },
		copy: func(c *Config) { c.Storage.ArtifactsPath = c.Artifacts.ArtifactsPath }},
	// Mirror the backend selector + S3 block from the alias section into the
	// canonical Storage block, so callers that read cfg.Storage see both shapes.
	{to: "storage.backend", from: "artifacts.backend",
		when: func(c *Config) bool { return c.Artifacts.Backend != "" && c.Storage.Backend == "" },
		copy: func(c *Config) { c.Storage.Backend = c.Artifacts.Backend }},
	{to: "storage.s3.endpoint", from: "artifacts.s3.endpoint",
		when: func(c *Config) bool { return c.Storage.S3.Endpoint == "" && c.Artifacts.S3.Endpoint != "" },
		copy: func(c *Config) { c.Storage.S3.Endpoint = c.Artifacts.S3.Endpoint }},
	{to: "storage.s3.region", from: "artifacts.s3.region",
		when: func(c *Config) bool { return c.Storage.S3.Region == "" && c.Artifacts.S3.Region != "" },
		copy: func(c *Config) { c.Storage.S3.Region = c.Artifacts.S3.Region }},
	{to: "storage.s3.bucket", from: "artifacts.s3.bucket",
		when: func(c *Config) bool { return c.Storage.S3.Bucket == "" && c.Artifacts.S3.Bucket != "" },
		copy: func(c *Config) { c.Storage.S3.Bucket = c.Artifacts.S3.Bucket }},
	{to: "storage.s3.prefix", from: "artifacts.s3.prefix",
		when: func(c *Config) bool { return c.Storage.S3.Prefix == "" && c.Artifacts.S3.Prefix != "" },
		copy: func(c *Config) { c.Storage.S3.Prefix = c.Artifacts.S3.Prefix }},
	{to: "storage.s3.access_key_id", from: "artifacts.s3.access_key_id",
		when: func(c *Config) bool { return c.Storage.S3.AccessKeyID == "" && c.Artifacts.S3.AccessKeyID != "" },
		copy: func(c *Config) { c.Storage.S3.AccessKeyID = c.Artifacts.S3.AccessKeyID }},
	{to: "storage.s3.secret_access_key", from: "artifacts.s3.secret_access_key",
		when: func(c *Config) bool {
			return c.Storage.S3.SecretAccessKey == "" && c.Artifacts.S3.SecretAccessKey != ""
		},
		copy: func(c *Config) { c.Storage.S3.SecretAccessKey = c.Artifacts.S3.SecretAccessKey }},
	{to: "storage.s3.use_path_style", from: "artifacts.s3.use_path_style",
		when: func(c *Config) bool { return !c.Storage.S3.UsePathStyle && c.Artifacts.S3.UsePathStyle },
		copy: func(c *Config) { c.Storage.S3.UsePathStyle = true }},
	{to: "storage.s3.force_ssl", from: "artifacts.s3.force_ssl",
		when: func(c *Config) bool { return c.Storage.S3.ForceSSL == nil && c.Artifacts.S3.ForceSSL != nil },
		copy: func(c *Config) { c.Storage.S3.ForceSSL = c.Artifacts.S3.ForceSSL }},
}

// secretFileResolver is one *_file -> inline secret resolution, for recording.
type secretFileResolver struct {
	target  string
	pathOf  func(c *Config) string
	resolve func(c *Config) error
}

var secretFileResolvers = []secretFileResolver{
	{target: "auth.providers.github.client_secret",
		pathOf: func(c *Config) string {
			if c.Auth.Providers.GitHub == nil {
				return ""
			}
			return c.Auth.Providers.GitHub.ClientSecretFile
		},
		resolve: resolveAuthSecrets},
	{target: "trading.auth.secret", pathOf: func(c *Config) string { return c.Trading.Auth.SecretFile }, resolve: resolveTradingSecret},
	{target: "gateway.token", pathOf: func(c *Config) string { return c.Gateway.TokenFile }, resolve: resolveGatewaySecret},
}

// LoadFromPathWithProvenance is LoadFromPath, recording where every value came
// from as the pipeline runs. On any error nothing is returned but the error —
// a partial map for a config that never loaded must not escape.
func LoadFromPathWithProvenance(path string) (*Config, *Provenance, error) {
	cfg := DefaultConfig()
	prov := newProvenance(path)

	// 1. Defaults: every non-zero leaf DefaultConfig set.
	WalkLeaves(reflect.ValueOf(cfg).Elem(), func(key string, _ reflect.StructField, v reflect.Value) {
		if !leafIsZero(v) {
			prov.set(key, OriginDefault, "")
		}
	})

	// 2. The file, if any: decode, and record presence.
	rawConfigBytes, err := decodeConfigFile(path, cfg, prov)
	if err != nil {
		return nil, nil, err
	}

	// 3. The artifacts.* alias block mirrored into storage.*.
	for _, m := range aliasMirrors {
		if m.when(cfg) {
			m.copy(cfg)
			prov.set(m.to, OriginAlias, m.from)
		}
	}

	// Source `<configDir>/secrets/*.env` into the process environment before
	// resolving placeholders + env overrides (see the function doc).
	sourceSecretsEnvFiles(path)

	// Recorded AFTER sourcing the env files and before expansion, which
	// destroys the evidence by turning ${VAR} into "".
	cfg.unresolvedPlaceholders = unresolvedPlaceholderNames(rawConfigBytes)

	// 4. Placeholders: leaves whose rendering carried ${VAR} and changed.
	recordChanged(cfg, prov, func(c *Config) { expandEnvPlaceholders(c) }, func(was string) (Origin, string, bool) {
		if !strings.Contains(was, "$") {
			return "", "", false
		}
		return OriginPlaceholder, placeholderVars(was), true
	})

	// 5. Environment overrides, from the table.
	applyEnvOverridesRecording(cfg, func(keys []string, env string, err error) {
		for _, k := range keys {
			if err != nil {
				prov.set(k, OriginEnvInvalid, fmt.Sprintf("%s (%v)", env, err))
				continue
			}
			prov.set(k, OriginEnv, env)
		}
	})

	// 6. Post-parse defaults: whatever they changed is derived.
	recordChanged(cfg, prov, func(c *Config) { applyAuthDefaults(c) }, derivedBy("applyAuthDefaults"))

	// 7. Secret files: record the basename before the resolver clears the path.
	for _, r := range secretFileResolvers {
		p := r.pathOf(cfg)
		if err := r.resolve(cfg); err != nil {
			return nil, nil, err
		}
		if p != "" {
			prov.set(r.target, OriginSecretFile, filepath.Base(p))
		}
	}

	recordChanged(cfg, prov, func(c *Config) { c.Composer.applyDefaults() }, derivedBy("composer.applyDefaults"))

	if err := cfg.Validate(); err != nil {
		return nil, nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	// 8. Everything else: unset if zero, derived if something set it without
	// telling us (a Validate side effect).
	WalkLeaves(reflect.ValueOf(cfg).Elem(), func(key string, _ reflect.StructField, v reflect.Value) {
		if _, ok := prov.Values[key]; ok {
			return
		}
		if leafIsZero(v) {
			prov.set(key, OriginUnset, "")
			return
		}
		prov.set(key, OriginDerived, "")
	})
	return cfg, prov, nil
}

// decodeConfigFile reads and decodes the file into cfg (no file: nothing to
// do) and records `file` for every leaf whose key the document carries.
// Returns the raw bytes for the placeholder diagnostic.
func decodeConfigFile(path string, cfg *Config, prov *Provenance) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	isJSON := strings.HasSuffix(path, ".json")
	if isJSON {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config: %w", err)
		}
	} else if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML config: %w", err)
	}
	present, err := presentKeys(data, isJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to index config keys: %w", err)
	}
	base := filepath.Base(path)
	WalkLeaves(reflect.ValueOf(cfg).Elem(), func(key string, _ reflect.StructField, _ reflect.Value) {
		if present[key] {
			prov.set(key, OriginFile, base)
		}
	})
	return data, nil
}

// recordChanged runs step and records, for every leaf whose rendering changed,
// the origin classify returns — the way a stage that does not know which
// fields it touches (post-parse defaults, placeholder expansion) is observed.
func recordChanged(cfg *Config, prov *Provenance, step func(*Config), classify func(was string) (Origin, string, bool)) {
	before := leafSnapshot(cfg)
	step(cfg)
	after := leafSnapshot(cfg)
	for key, was := range before {
		if after[key] == was {
			continue
		}
		if o, src, ok := classify(was); ok {
			prov.set(key, o, src)
		}
	}
}

func derivedBy(source string) func(string) (Origin, string, bool) {
	return func(string) (Origin, string, bool) { return OriginDerived, source, true }
}
