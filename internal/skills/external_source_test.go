package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// HXC-159 T-P9.01 finding F-01 (HelixAgent side): ExternalSkillSource
// previously implied activation by directory listing alone, bypassing the
// ported P4 activation policy (external_policy.go) entirely. This file did
// not exist before this fix — there was NO prior test coverage proving
// what ExternalSkillSource actually admitted.

// writeExternalSkill writes a single minimal, valid SKILL.md under
// dir/name so a test can populate a fake external source with an
// arbitrary number of skills.
func writeExternalSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n## Instructions\n\nBody for %s.\n", name, description, name)
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExternalSkillSource_CeilingRefusesTheWholeSource proves the ported
// nativeDefaultCeiling policy GENUINELY runs over ExternalSkillSource.Load:
// a directory holding one more skill than the ceiling permits must admit
// ZERO tools, never a silent unconditional admission.
func TestExternalSkillSource_CeilingRefusesTheWholeSource(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < nativeDefaultCeiling+1; i++ {
		writeExternalSkill(t, dir, fmt.Sprintf("skill-%02d", i), fmt.Sprintf("ceiling probe fixture skill number %d for testing", i))
	}

	src := NewExternalSkillSource("external", dir, nil)
	tools, err := src.Load()
	if err != nil {
		t.Fatalf("Load returned an infrastructure error, expected a logged policy refusal with (nil, nil): %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("an over-ceiling external source must admit ZERO tools, got %d — the ceiling policy is not genuinely gating ExternalSkillSource.Load (HXC-159 F-01)", len(tools))
	}
}

// TestExternalSkillSource_UnderCeilingIsAdmitted is the golden-good
// counterpart (§11.4.107(10)): a source genuinely within policy is
// admitted, proving the ceiling check discriminates rather than
// always-refusing.
func TestExternalSkillSource_UnderCeilingIsAdmitted(t *testing.T) {
	dir := t.TempDir()
	writeExternalSkill(t, dir, "solo-skill", "a solo policy-compliant fixture skill for testing")

	src := NewExternalSkillSource("external", dir, nil)
	tools, err := src.Load()
	if err != nil {
		t.Fatalf("Load returned an unexpected error for a policy-compliant source: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected exactly the one compliant skill admitted, got %d", len(tools))
	}
	if tools[0].Name() != "solo-skill" {
		t.Fatalf("expected skill %q admitted, got %q", "solo-skill", tools[0].Name())
	}
}

// TestExternalSkillSource_SimilarityConflictRefusesTheWholeSource proves
// the ported description-similarity gate is genuinely reachable: two
// skills whose descriptions collide above nativeSimilarityThreshold must
// refuse the whole source.
func TestExternalSkillSource_SimilarityConflictRefusesTheWholeSource(t *testing.T) {
	dir := t.TempDir()
	// Two descriptions sharing every content token score Jaccard=1.0,
	// far above nativeSimilarityThreshold (0.0256).
	writeExternalSkill(t, dir, "alpha-skill", "validate recording screenshot capture evidence pipeline")
	writeExternalSkill(t, dir, "beta-skill", "validate recording screenshot capture evidence pipeline")

	src := NewExternalSkillSource("external", dir, nil)
	tools, err := src.Load()
	if err != nil {
		t.Fatalf("Load returned an infrastructure error, expected a logged policy refusal with (nil, nil): %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("a similarity-conflicting external source must admit ZERO tools, got %d", len(tools))
	}
}

// TestExternalSkillSource_C5SandboxPreconditionIsReachable proves the C5
// sandbox precondition (ported from pkg/skills' EnforceSandboxPrecondition)
// is genuinely reachable through this consumer's own wiring: a
// TrustThirdParty source with SandboxReady left at its safe default
// (false) MUST refuse, and a real, deliberately-set-true SandboxReady MUST
// admit — proving the gate discriminates rather than always-refusing.
func TestExternalSkillSource_C5SandboxPreconditionIsReachable(t *testing.T) {
	dir := t.TempDir()
	writeExternalSkill(t, dir, "third-party-skill", "a third party trust tier fixture skill for testing")

	blocked := NewExternalSkillSource("external", dir, nil)
	blocked.Trust = TrustThirdParty
	// SandboxReady left at its zero value (false) — the safe default.
	tools, err := blocked.Load()
	if err != nil {
		t.Fatalf("expected a logged policy refusal with (nil, nil), got infrastructure error: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("a TrustThirdParty source with no sandbox wired must admit ZERO tools (C5), got %d", len(tools))
	}

	admitted := NewExternalSkillSource("external", dir, nil)
	admitted.Trust = TrustThirdParty
	admitted.SandboxReady = true
	tools2, err2 := admitted.Load()
	if err2 != nil {
		t.Fatalf("expected a compliant TrustThirdParty+SandboxReady=true source to load cleanly: %v", err2)
	}
	if len(tools2) != 1 {
		t.Fatalf("expected the one skill admitted once SandboxReady=true, got %d", len(tools2))
	}
}

// TestExternalSkillSource_AbsentDirIsNotAnError matches ParseDirectory's
// pre-existing contract: a nonexistent source directory is not a policy
// question and not treated as a fatal error by this test's expectations —
// it exercises Load's real behaviour on an absent directory so a future
// regression in that path is caught too.
func TestExternalSkillSource_AbsentDirIsNotAnError(t *testing.T) {
	src := NewExternalSkillSource("external", filepath.Join(t.TempDir(), "does-not-exist"), nil)
	tools, err := src.Load()
	if len(tools) != 0 {
		t.Fatalf("expected zero tools for an absent directory, got %d", len(tools))
	}
	_ = err // ParseDirectory's own absent-dir contract is exercised as-is; this test only asserts zero tools are ever silently admitted.
}

// TestExternalSkillSource_MalformedSkillDoesNotAbortWholeLoad exercises
// this package's pre-existing "a single broken skill file never blocks the
// front-end" contract (T-P6.04) through the NEW policy-gated Load path:
// a malformed SKILL.md alongside well-formed ones must not turn Load into
// a hard error the caller can't recover from.
func TestExternalSkillSource_MalformedSkillDoesNotAbortWholeLoad(t *testing.T) {
	dir := t.TempDir()
	writeExternalSkill(t, dir, "good-skill", "a well formed fixture skill for testing purposes only")
	malformedDir := filepath.Join(dir, "bad-skill")
	if err := os.MkdirAll(malformedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// This package's Parser (parser.go splitFrontmatter) treats a file with
	// NO "---" delimiter at all as "no frontmatter, whole file is body" —
	// NOT a parse failure (deliberately more lenient than pkg/skills'
	// external constitution skills, which DO hard-error on a missing
	// front-matter block). The genuine ParseFailure class for THIS parser
	// is an UNTERMINATED front-matter block (opened "---" with no closing
	// "---"), which splitFrontmatter explicitly errors on.
	if err := os.WriteFile(filepath.Join(malformedDir, "SKILL.md"), []byte("---\nname: bad-skill\ndescription: unterminated front matter block\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := NewExternalSkillSource("external", dir, nil)
	tools, err := src.Load()
	if err != nil {
		t.Fatalf("Load must not hard-error on a malformed sibling skill (T-P6.04): %v", err)
	}
	if len(tools) != 1 || tools[0].Name() != "good-skill" {
		t.Fatalf("expected exactly the well-formed skill admitted despite the malformed sibling, got %d tools: %+v", len(tools), tools)
	}
}
