package skills

import (
	"context"
	"fmt"

	"dev.helix.agent/internal/services"
)

// ExternalSkillSource bridges a directory of SKILL.md manifests into BOTH
// halves of HelixAgent's skill surface (HXC-159 T-P6.01, attachment point
// rank 2 per docs/research/.../03_consumer_integration_surfaces.md §6):
//
//  1. services.ToolRegistry, via the fetcher shape
//     RegisterExternalToolSource(name, fetcher) expects — this is the
//     "cleanest generic hook in either module": name + closure, gets
//     validation, dedup, unified Search()/GetToolStats() for free, and
//     (T-P6.01.3) survives RefreshTools.
//  2. skills.Service / skills.Registry, which is what ACTUALLY backs the
//     four HTTP routes (GET /v1/skills, /categories, /:category, POST
//     /match — RS-10). ProtocolSkillAdapter only converts Service-side
//     skills OUT to MCP/ACP/LSP shapes; it never feeds ToolRegistry
//     registrations back IN. Without step 2, a source registered ONLY via
//     RegisterExternalToolSource would validate/dedup/search inside
//     ToolRegistry yet remain invisible to every existing skills HTTP
//     consumer — RS-10 would never be satisfied by step 1 alone.
//
// D-4 (decision, spec.md): this reads the source directory as PLAIN DATA
// (os.ReadDir / SKILL.md parsing via this package's own Parser) — it never
// imports github.com/HelixDevelopment/skills as a Go module. See
// tests/compliance/module_graph_edge_test.go (T-P6.05) for the mechanical
// gate that enforces the module graph never grows that edge.
//
// HXC-159 T-P9.01/F-01: Load() previously implied activation by returning
// every skill ParseDirectory discovered, with none of the P4 activation
// policy (allowlist, activation ceiling, description-similarity gate, C5
// sandbox precondition) ever consulted — that policy existed only inside
// github.com/HelixDevelopment/skills, a library this package structurally
// cannot import (D-4 above). external_policy.go is a NATIVE,
// non-importing reimplementation of that SAME policy (ported verbatim from
// pkg/skills/activate.go + similarity.go + sandbox_gate.go, cited there),
// and Load() now runs every discovered skill through it before returning
// or registering anything.
type ExternalSkillSource struct {
	// Name identifies the source (the sourceName RegisterExternalToolSource
	// and every Tool.Source() will report).
	Name string
	// Dir is the directory ExternalSkillSource walks for SKILL.md files —
	// config-injected by the caller (CONST-051(B): this package stays
	// project-not-aware; it never hardcodes a path).
	Dir string
	// Trust classifies this source for the C5 sandbox precondition
	// (HXC-159 F-01). Config-injected by the caller; the zero value is
	// TrustVendored, matching this consumer's pre-existing self-report in
	// internal/skillconformance/report.go's trustForTier("external").
	Trust ExternalSourceTrust
	// SandboxReady declares whether a REAL (non-skip) isolated executor
	// backs this consumer's third-party-tier skill execution path (C5
	// precondition). Config-injected, never hardcoded true: HelixAgent has
	// not wired a real sandbox executor as of this fix, so callers MUST
	// leave this false — hardcoding true here would be exactly the bluff
	// F-02 closed inside pkg/skills itself.
	SandboxReady bool

	parser  *Parser
	service *Service // may be nil: Load() then registers ToolRegistry-only
}

// NewExternalSkillSource constructs a source rooted at dir, defaulting to
// TrustVendored with SandboxReady=false (HXC-159 F-01's safe default —
// callers needing TrustThirdParty or a wired sandbox set ExternalSkillSource
// 's exported fields directly after construction). service is the
// skills.Service backing the consumer's HTTP routes; pass nil to register
// only into a services.ToolRegistry (e.g. a caller that has no skills.Service
// of its own) — Load() still returns the fetched Tools in that case, it
// simply skips the Service-side registration half of the bridge.
func NewExternalSkillSource(name, dir string, service *Service) *ExternalSkillSource {
	return &ExternalSkillSource{
		Name:    name,
		Dir:     dir,
		Trust:   TrustVendored,
		parser:  NewParser(),
		service: service,
	}
}

// Load parses ExternalSkillSource.Dir (recursively, via this package's own
// Parser — T-P6.04's malformed-skill surfacing applies here too, since
// ParseDirectory is shared) and returns each discovered skill wrapped as a
// services.Tool: the shape RegisterExternalToolSource's fetcher parameter
// requires. As a side effect (the bridge's whole purpose) every discovered
// skill is ALSO registered into the backing skills.Service, if one was
// supplied, so HTTP consumers of GET /v1/skills (and its /categories,
// /:category, POST /match siblings) observe it too.
//
// Load is the fetcher closure RegisterWith passes to
// ToolRegistry.RegisterExternalToolSource — it is called once at
// registration time and again on every RefreshTools (T-P6.01.3), so a
// skill added to Dir after startup is picked up on the next refresh without
// restarting the process.
func (s *ExternalSkillSource) Load() ([]services.Tool, error) {
	discovered, failures, err := s.parser.ParseDirectory(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("skills: external source %q: %w", s.Name, err)
	}
	// T-P6.04 direction: malformed skills within an external source are
	// surfaced (ParseDirectory already logged each at Warn), not silently
	// dropped from the load — Load() itself does not error on them so the
	// well-formed skills in the same source still register.
	_ = failures

	// HXC-159 T-P9.01/F-01: every discovered skill MUST cross the native
	// policy gate (ceiling + description-similarity + C5 sandbox
	// precondition, external_policy.go) before it is exposed as a Tool or
	// registered into the backing skills.Service — discovery on disk no
	// longer implies activation. A policy refusal admits ZERO skills from
	// this Load() call (fail-closed over the whole source, matching
	// pkg/skills.Registry.Activate's own all-or-nothing semantics) and is
	// logged, not returned as an error — the caller's pre-existing
	// contract (a malformed/refused external source never blocks other
	// tiers the caller also loads) is preserved.
	admitted, refusal := externalPolicyGate(s.Name, discovered, s.Trust, s.SandboxReady)
	if refusal != nil {
		logPolicyRefusal(s.Name, refusal)
		return nil, nil
	}

	tools := make([]services.Tool, 0, len(admitted))
	for _, sk := range admitted {
		if s.service != nil {
			s.service.RegisterSkill(sk)
		}
		tools = append(tools, &skillToolAdapter{skill: sk, source: s.Name})
	}
	return tools, nil
}

// RegisterWith wires this source into registry via
// ToolRegistry.RegisterExternalToolSource (T-P6.01.2), using Load as the
// fetcher closure.
func (s *ExternalSkillSource) RegisterWith(registry *services.ToolRegistry) error {
	return registry.RegisterExternalToolSource(s.Name, s.Load)
}

// skillToolAdapter adapts a *Skill to the services.Tool interface so it
// participates in ToolRegistry's unified validation/dedup/search/stats.
type skillToolAdapter struct {
	skill  *Skill
	source string
}

func (a *skillToolAdapter) Name() string        { return a.skill.Name }
func (a *skillToolAdapter) Description() string { return a.skill.Description }

func (a *skillToolAdapter) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"query": map[string]interface{}{
			"type":        "string",
			"description": "The query or prompt for the skill",
		},
	}
}

func (a *skillToolAdapter) Execute(_ context.Context, _ map[string]interface{}) (interface{}, error) {
	return a.skill.Instructions, nil
}

func (a *skillToolAdapter) Source() string { return a.source }
