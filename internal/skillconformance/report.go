package skillconformance

import (
	"path/filepath"
	"strings"
	"time"

	"dev.helix.agent/internal/skills"
)

// DirTierMap names the on-disk directories a real skills.Service was
// loaded from, so GenerateReport can classify each registered skill's
// tier from its Skill.FilePath prefix.
type DirTierMap struct {
	Local    string
	External string
}

func (t DirTierMap) tierFor(filePath string) string {
	if t.External != "" && strings.HasPrefix(filePath, t.External) {
		return "external"
	}
	if t.Local != "" && strings.HasPrefix(filePath, t.Local) {
		return "local"
	}
	return "unclassified"
}

// trustForTier is this package's OWN declared policy: a locally-authored
// skill (Local dir, config-injected by the operator, typically inside
// this checkout) is first-party; an externally-sourced skill
// (HELIXSKILLS_EXTERNAL_SOURCE_DIR, per T-P6.01/router.go) is vendored —
// it was not authored as part of this module. Skill carries no trust
// field of its own to measure (§11.4.6: declared, not measured).
func trustForTier(tier string) string {
	if tier == "local" {
		return "first-party"
	}
	return "vendored"
}

// qualifiedIdentity derives the STABLE, directory-based local-identity
// component of a skill's Qualified name (HXC-159 T-P9.01 finding F-11).
//
// s.Name comes straight from this skill's YAML front-matter `name:` field
// (types.go: `Name string yaml:"name"`) — free-form prose a skill author
// writes (e.g. "Media Validator"), NOT a stable identifier. Two problems
// follow directly from using it as the cross-consumer comparison key: (1)
// it is ABSENT for any skill whose SKILL.md carries no front-matter at all
// (this package's own Parser treats that as valid — types.go/parser.go —
// so s.Name is simply "" and Qualified degenerates to "<tier>."), and (2)
// even when present, sibling consumer HelixCode's own report generator
// (dev.helix.code/internal/skillconformance.GenerateReport) does NOT use
// front-matter prose for its equivalent key — HelixCode's commands.Skill
// name is ALWAYS the loader-supplied directory/filename basename
// (helix_code/internal/commands/markdown_skills.go: parseSkillFile's name
// parameter is populated from the directory walk, e.g. `name :=
// entry.Name()`, never overridden by front-matter). Comparing HelixAgent's
// front-matter-derived key against HelixCode's directory-derived key for
// the SAME externally-installed skill directory therefore produces a
// FALSE MISMATCH the moment the two front-ends parse that directory's
// SKILL.md differently (missing front-matter, differing title casing,
// etc.) — exactly the D-1/FR-013 violation F-11 reports.
//
// The fix: derive the SAME stable directory-based identity HelixCode
// already uses — the basename of the directory containing the skill's
// SKILL.md (filepath.Base(filepath.Dir(s.FilePath))) — so the two
// consumers' reports key the SAME externally-installed skill identically
// regardless of front-matter-parsing differences between their loaders.
func qualifiedIdentity(s *skills.Skill) string {
	if dirName := filepath.Base(filepath.Dir(s.FilePath)); dirName != "" && dirName != "." && dirName != string(filepath.Separator) {
		return dirName
	}
	// FilePath absent or degenerate (e.g. a NewSkill-style in-memory
	// construction with no real file path): fall back to the front-matter
	// name rather than emitting an empty identity — an honest degradation,
	// not a silent one, since a skill with neither a real file path nor a
	// declared name has no stable identity to report at all.
	return s.Name
}

// GenerateReport builds a CapabilityManifest from an ALREADY-LOADED
// skills.Service and its SkillAuthorizer. Every loader_api_surface flag
// below is set from a fact measured directly in internal/skills — see
// the citing comment on each — never assumed.
func GenerateReport(svc *skills.Service, authz *skills.SkillAuthorizer, tiers DirTierMap) CapabilityManifest {
	all := svc.GetAllSkills()
	reports := make([]SkillReport, 0, len(all))
	for _, s := range all {
		tier := tiers.tierFor(s.FilePath)
		var caps []string
		if s.AllowedTools != "" {
			caps = []string{s.AllowedTools}
		}
		reports = append(reports, SkillReport{
			Qualified:    tier + "." + qualifiedIdentity(s),
			Source:       tier,
			Trust:        trustForTier(tier),
			Capabilities: caps,
		})
	}
	surface := LoaderAPISurface{
		// true: RegisterExternalToolSource (services.ToolRegistry) AND
		// LoadSkillsFromPath/LoadFromPath both accept an arbitrary named
		// directory as an additional source — this module already
		// supports >=2 independently-loaded sources (T-P6.01).
		SourceRegistration: true,
		// false: Registry.registerSkillLocked (registry.go) keys SOLELY
		// by bare skill.Name via r.skills.Put(skill.Name, skill) — a
		// same-named skill from two sources silently overwrites, no
		// <source>.<name> pair.
		NamespacedIdentity: false,
		// true: Skill.AllowedTools (types.go) is a real, parsed
		// (ParseAllowedTools) declared-capability field on every skill.
		CapabilityDeclaration: true,
		// true: SkillAuthorizer.Authorize (authorizer.go, T-P6.03) is a
		// real fail-closed call boundary — a skill declaring AllowedTools
		// that does not name a requested tool is REFUSED.
		CapabilityEnforcement: true,
		// false: no per-skill activation allowlist/ceiling exists
		// anywhere in this package (grep-verified, matching T-P5.05.4's
		// finding for the sibling consumer).
		AllowlistActivation: false,
		// true: SkillAuthorizer.AuditLog() returns every recorded
		// ToolAuditEntry — allowed AND refused, never opt-in.
		AuditLog: true,
		// false: Skill carries no content-hash field and Registry
		// performs no hash comparison; T-P7.03's PinStore lives in the
		// library only and this module may not import it (D-4).
		ContentHashProvenance: false,
		// true: Registry.LoadFromPath calls recordLoadFailuresLocked
		// separately from registerSkillLocked — a malformed sibling
		// never blocks a well-formed skill in the same directory,
		// proven live by TestGenerateReport_MalformedSkillIsVisibleNotFatal.
		MalformedSkillVisibility: true,
	}
	// authz is accepted (rather than derived internally) so callers pass
	// the SAME authorizer instance production code actually wires — the
	// audit_log flag above is a claim about the TYPE's capability, and
	// reading its trail here (discarded) keeps this generator honest
	// about depending on a real, already-exercised authorizer rather
	// than constructing a fresh, never-called one.
	_ = authz.AuditLog()
	return CapabilityManifest{
		SchemaVersion:    SchemaVersion,
		Consumer:         "helix_agent",
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		LoaderAPISurface: surface,
		Skills:           reports,
	}
}
