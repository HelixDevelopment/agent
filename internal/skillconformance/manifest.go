// Package skillconformance implements HelixAgent's OWN half of the
// HXC-159 T-P7.01/T-P7.02 cross-consumer capability-manifest contract.
//
// D-4 (specs/001-helixskills-incorporation/spec.md, meta-repo) is
// absolute for this module: HelixAgent is a CLIENT of a skills source,
// never an importer of github.com/HelixDevelopment/skills — the
// module-graph edge gate (T-P6.05, tests/compliance/module_graph_edge_test.go)
// mechanically enforces zero such edge. The capability manifest is
// therefore a WIRE SCHEMA — see
// submodules/skills/docs/conformance/capability_manifest.schema.json in
// the meta-repo and its Go reference implementation
// pkg/skills/conformance.go — that this package reimplements
// independently, field-for-field, entirely inside dev.helix.agent. "The
// same schema, two independent implementations, zero shared import" IS
// the conformance proof D-3/D-4 require.
package skillconformance

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion mirrors github.com/HelixDevelopment/skills/pkg/skills's
// ConformanceSchemaVersion. Bumped only on a breaking wire-shape change.
const SchemaVersion = "1.0"

var knownConsumers = map[string]bool{
	"library":     true,
	"helix_code":  true,
	"helix_agent": true,
}

var knownTrustTiers = map[string]bool{
	"first-party": true,
	"vendored":    true,
	"third-party": true,
}

// LoaderAPISurface — field-for-field identical to the shared schema.
type LoaderAPISurface struct {
	SourceRegistration       bool `json:"source_registration"`
	NamespacedIdentity       bool `json:"namespaced_identity"`
	CapabilityDeclaration    bool `json:"capability_declaration"`
	CapabilityEnforcement    bool `json:"capability_enforcement"`
	AllowlistActivation      bool `json:"allowlist_activation"`
	AuditLog                 bool `json:"audit_log"`
	ContentHashProvenance    bool `json:"content_hash_provenance"`
	MalformedSkillVisibility bool `json:"malformed_skill_visibility"`
}

// SkillReport — field-for-field identical to the shared schema.
type SkillReport struct {
	Qualified    string   `json:"qualified_name"`
	Source       string   `json:"source"`
	Trust        string   `json:"trust_tier"`
	Capabilities []string `json:"capabilities,omitempty"`
	Hash         string   `json:"hash,omitempty"`
}

// OptionalCapability — field-for-field identical to the shared schema.
type OptionalCapability struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// CapabilityManifest — field-for-field identical to the shared schema.
type CapabilityManifest struct {
	SchemaVersion        string               `json:"schema_version"`
	Consumer             string               `json:"consumer"`
	GeneratedAt          string               `json:"generated_at"`
	LoaderAPISurface     LoaderAPISurface     `json:"loader_api_surface"`
	Skills               []SkillReport        `json:"skills"`
	OptionalCapabilities []OptionalCapability `json:"optional_capabilities,omitempty"`
}

// Validate checks m against the shared schema and returns every violation
// found. Deliberately re-implemented (never imported) from the library's
// pkg/skills/conformance.go — see the package doc comment.
func (m CapabilityManifest) Validate() []error {
	var errs []error
	if m.SchemaVersion == "" {
		errs = append(errs, fmt.Errorf("schema_version is required"))
	} else if m.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("schema_version %q does not match the known schema %q", m.SchemaVersion, SchemaVersion))
	}
	if m.Consumer == "" {
		errs = append(errs, fmt.Errorf("consumer is required"))
	} else if !knownConsumers[m.Consumer] {
		errs = append(errs, fmt.Errorf("consumer %q is not one of the known set (library, helix_code, helix_agent)", m.Consumer))
	}
	if m.GeneratedAt == "" {
		errs = append(errs, fmt.Errorf("generated_at is required"))
	} else if _, err := time.Parse(time.RFC3339, m.GeneratedAt); err != nil {
		errs = append(errs, fmt.Errorf("generated_at %q is not RFC3339: %w", m.GeneratedAt, err))
	}
	seen := map[string]bool{}
	for i, sk := range m.Skills {
		if sk.Qualified == "" {
			errs = append(errs, fmt.Errorf("skills[%d].qualified_name is required", i))
		} else if !strings.Contains(sk.Qualified, ".") {
			errs = append(errs, fmt.Errorf("skills[%d].qualified_name %q must be namespaced <source>.<name>", i, sk.Qualified))
		} else if seen[sk.Qualified] {
			errs = append(errs, fmt.Errorf("skills[%d].qualified_name %q is a duplicate within this manifest", i, sk.Qualified))
		}
		seen[sk.Qualified] = true
		if sk.Source == "" {
			errs = append(errs, fmt.Errorf("skills[%d].source is required", i))
		}
		if !knownTrustTiers[sk.Trust] {
			errs = append(errs, fmt.Errorf("skills[%d].trust_tier %q is not one of first-party/vendored/third-party", i, sk.Trust))
		}
	}
	for i, oc := range m.OptionalCapabilities {
		if oc.Key == "" {
			errs = append(errs, fmt.Errorf("optional_capabilities[%d].key is required", i))
		}
		if oc.Reason == "" {
			errs = append(errs, fmt.Errorf("optional_capabilities[%d].reason is required", i))
		}
	}
	return errs
}

// MarshalIndentJSON is a small convenience the CLI and tests share.
func (m CapabilityManifest) MarshalIndentJSON() ([]byte, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
