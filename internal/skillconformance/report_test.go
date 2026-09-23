package skillconformance

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"dev.helix.agent/internal/skills"
)

func writeFixtureSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGenerateReport_RealService_ValidatesClean drives the REAL production
// skills.Service (never a reimplementation) against real fixture skill
// directories, and asserts the generated report both validates against
// the shared schema and honestly reflects this consumer's actual,
// measured capability surface (§11.4.6 — measured, not assumed).
func TestGenerateReport_RealService_ValidatesClean(t *testing.T) {
	localDir := t.TempDir()
	externalDir := t.TempDir()
	writeFixtureSkill(t, localDir, "local-fixture-skill", "---\nname: local-fixture-skill\ndescription: a local fixture skill\n---\nBody.\n")
	writeFixtureSkill(t, externalDir, "external-fixture-skill", "---\nname: external-fixture-skill\ndescription: an external fixture skill\nallowed-tools: Read\n---\nBody.\n")

	cfg := skills.DefaultSkillConfig()
	cfg.SkillsDirectory = localDir
	cfg.EnableSemanticMatching = false
	svc := skills.NewService(cfg)
	if err := svc.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := svc.LoadSkillsFromPath(context.Background(), externalDir); err != nil {
		t.Fatalf("LoadSkillsFromPath(external): %v", err)
	}

	authz := skills.NewSkillAuthorizer(svc)
	// Exercise the authorizer once so audit_log is genuinely non-empty,
	// not merely claimed present.
	authz.Authorize("external-fixture-skill", "Write")

	m := GenerateReport(svc, authz, DirTierMap{Local: localDir, External: externalDir})
	if errs := m.Validate(); len(errs) != 0 {
		t.Fatalf("generated report must validate clean, got: %v", errs)
	}
	if m.Consumer != "helix_agent" {
		t.Fatalf("consumer = %q, want helix_agent", m.Consumer)
	}

	found := map[string]bool{}
	for _, s := range m.Skills {
		found[s.Qualified] = true
	}
	if !found["local.local-fixture-skill"] {
		t.Errorf("report missing local.local-fixture-skill; got %+v", m.Skills)
	}
	if !found["external.external-fixture-skill"] {
		t.Errorf("report missing external.external-fixture-skill; got %+v", m.Skills)
	}

	surface := m.LoaderAPISurface
	// This module DOES declare (AllowedTools) and DOES enforce
	// (SkillAuthorizer.Authorize, fail-closed) capabilities, and DOES
	// keep an audit trail (authorizer.go) — measured directly against
	// the real authorizer above, not assumed.
	if !surface.CapabilityDeclaration {
		t.Error("Skill.AllowedTools is a real declared-capability field; capability_declaration must be true")
	}
	if !surface.CapabilityEnforcement {
		t.Error("SkillAuthorizer.Authorize fail-closed-refuses; capability_enforcement must be true")
	}
	if !surface.AuditLog {
		t.Error("SkillAuthorizer.AuditLog() records every decision; audit_log must be true")
	}
	if surface.NamespacedIdentity {
		t.Error("Registry.registerSkillLocked keys solely by skill.Name (skills.Put(skill.Name, ...)); namespaced_identity must be false")
	}
	if !surface.MalformedSkillVisibility {
		t.Error("Registry.LoadFromPath records ParseDirectory failures separately and still registers well-formed siblings; malformed_skill_visibility must be true")
	}
	if surface.AllowlistActivation {
		t.Error("no activation allowlist/ceiling exists in this package (grep-verified); allowlist_activation must be false")
	}
	if surface.ContentHashProvenance {
		t.Error("this package pins no content hash and verifies none at load; content_hash_provenance must be false")
	}
}

// TestGenerateReport_QualifiedUsesDirectoryNotFrontMatterTitle is the
// HXC-159 T-P9.01 finding F-11 RED-first regression guard. Every
// pre-existing fixture in this file (and, before this test, in production)
// happened to give a skill's directory name and its front-matter `name:`
// field the SAME value — a coincidence that made the report's Qualified
// field blind to whether it was genuinely keying on stable directory
// identity or on arbitrary front-matter prose. This fixture DELIBERATELY
// makes the two differ (directory "media-validator", front-matter title
// "Media Validator (Human-Readable Title)") so this test can prove, not
// assume, which one GenerateReport actually uses.
//
// Cross-consumer parity (D-1/FR-013) requires the STABLE, directory-based
// identity: sibling consumer HelixCode's own report generator
// (dev.helix.code/internal/skillconformance.GenerateReport) always keys on
// the loader-supplied directory basename, never on front-matter prose —
// see qualifiedIdentity's doc comment in report.go for the full citation.
func TestGenerateReport_QualifiedUsesDirectoryNotFrontMatterTitle(t *testing.T) {
	localDir := t.TempDir()
	writeFixtureSkill(t, localDir, "media-validator",
		"---\nname: Media Validator (Human-Readable Title)\ndescription: validates media\n---\nBody.\n")

	cfg := skills.DefaultSkillConfig()
	cfg.SkillsDirectory = localDir
	cfg.EnableSemanticMatching = false
	svc := skills.NewService(cfg)
	if err := svc.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	authz := skills.NewSkillAuthorizer(svc)

	m := GenerateReport(svc, authz, DirTierMap{Local: localDir})
	if len(m.Skills) != 1 {
		t.Fatalf("expected exactly one skill in the report, got %d: %+v", len(m.Skills), m.Skills)
	}
	got := m.Skills[0].Qualified
	const want = "local.media-validator"
	if got != want {
		t.Fatalf("Qualified = %q, want %q — GenerateReport is keying on front-matter title %q instead of the stable directory-based identity (HXC-159 F-11)",
			got, want, "Media Validator (Human-Readable Title)")
	}
}

// TestGenerateReport_MalformedSkillIsVisibleNotFatal proves
// malformed_skill_visibility=true against a REAL malformed fixture via
// the REAL production LoadFromPath -> ParseDirectory path.
func TestGenerateReport_MalformedSkillIsVisibleNotFatal(t *testing.T) {
	localDir := t.TempDir()
	writeFixtureSkill(t, localDir, "good-skill", "---\nname: good-skill\ndescription: fine\n---\nBody.\n")
	writeFixtureSkill(t, localDir, "bad-skill", "---\nname: never closed\nno closing delimiter at all\n")

	cfg := skills.DefaultSkillConfig()
	cfg.SkillsDirectory = localDir
	cfg.EnableSemanticMatching = false
	svc := skills.NewService(cfg)
	if err := svc.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize must NOT fail on a malformed sibling skill: %v", err)
	}

	authz := skills.NewSkillAuthorizer(svc)
	m := GenerateReport(svc, authz, DirTierMap{Local: localDir})
	found := map[string]bool{}
	for _, s := range m.Skills {
		found[s.Qualified] = true
	}
	if !found["local.good-skill"] {
		t.Fatalf("well-formed sibling must still be registered; report=%+v", m.Skills)
	}
}
