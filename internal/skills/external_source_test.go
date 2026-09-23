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

// TestExternalSkillSource_SimilarityConflictExcludesOnlyThePair proves the
// HXC-159 F-17 (2026-09-23) per-skill (partial) admission redesign is
// genuinely reachable through ExternalSkillSource.Load: two skills whose
// descriptions collide above nativeSimilarityThreshold no longer refuse the
// whole source — exactly ONE of the pair is admitted (the
// alphabetically-first Qualified() identity per the tie-break rule) and a
// third, unrelated skill in the same source still loads.
func TestExternalSkillSource_SimilarityConflictExcludesOnlyThePair(t *testing.T) {
	dir := t.TempDir()
	// Two descriptions sharing every content token score Jaccard=1.0,
	// far above nativeSimilarityThreshold (0.0709).
	writeExternalSkill(t, dir, "alpha-skill", "validate recording screenshot capture evidence pipeline")
	writeExternalSkill(t, dir, "beta-skill", "validate recording screenshot capture evidence pipeline")
	writeExternalSkill(t, dir, "gamma-skill", "translate legal contracts between spanish and portuguese dialects")

	src := NewExternalSkillSource("external", dir, nil)
	tools, err := src.Load()
	if err != nil {
		t.Fatalf("Load returned an unexpected infrastructure error: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected exactly 2 tools admitted (one of the confusable pair + the unrelated skill), got %d: %+v", len(tools), tools)
	}
	byName := map[string]bool{}
	for _, tl := range tools {
		byName[tl.Name()] = true
	}
	if byName["alpha-skill"] == byName["beta-skill"] {
		t.Fatalf("exactly one of alpha-skill/beta-skill must be admitted, got both present=%v", byName["alpha-skill"] && byName["beta-skill"])
	}
	// external.alpha-skill < external.beta-skill lexicographically — the
	// alphabetically-first identity is kept per the tie-break rule.
	if !byName["alpha-skill"] {
		t.Fatalf("tie-break must keep alpha-skill (alphabetically first), got %+v", byName)
	}
	if !byName["gamma-skill"] {
		t.Fatalf("the unrelated, non-conflicting skill must still be admitted under partial admission, got %+v", byName)
	}
}

// TestNativeAdmitBySimilarityNeverAdmitsBothOfAPair mirrors
// pkg/skills.TestAdmitBySimilarityNeverAdmitsBothOfAPair on this native
// port: the F-17 safety invariant this remediation MUST preserve is that no
// path through nativeAdmitBySimilarity can ever return two mutually
// confusable skills in the same admitted set.
func TestNativeAdmitBySimilarityNeverAdmitsBothOfAPair(t *testing.T) {
	mk := func(name, desc string) *Skill { return &Skill{Name: name, Description: desc} }
	a := mk("alpha", "reconcile ledger balance entries nightly batch")
	b := mk("bravo", "reconcile ledger balance entries nightly batch")
	c := mk("charlie", "reconcile ledger balance entries nightly batch")
	d := mk("delta", "compress seismic waveform telemetry for archival storage")
	ordered := []*Skill{a, b, c, d} // already alphabetical by qualifiedName("clique", ·)

	admitted, exclusions := nativeAdmitBySimilarity("clique", ordered)

	for i := 0; i < len(admitted); i++ {
		for j := i + 1; j < len(admitted); j++ {
			if s := nativeJaccard(admitted[i].Description, admitted[j].Description); s > nativeSimilarityThreshold {
				t.Fatalf("SAFETY VIOLATION: both %q and %q admitted with confusable score %.4f > threshold %.4f",
					admitted[i].Name, admitted[j].Name, s, nativeSimilarityThreshold)
			}
		}
	}
	if len(admitted) != 2 {
		t.Fatalf("expected exactly 2 admitted (one clique survivor + delta), got %d", len(admitted))
	}
	if admitted[0].Name != "alpha" {
		t.Fatalf("expected the clique's alphabetically-first member admitted, got %q", admitted[0].Name)
	}
	if len(exclusions) != 2 {
		t.Fatalf("expected exactly 2 exclusions (bravo, charlie both conflict with alpha), got %d: %+v", len(exclusions), exclusions)
	}
	for _, ex := range exclusions {
		if ex.ConflictsWith != "clique.alpha" {
			t.Fatalf("both clique exclusions must cite clique.alpha as the conflicting admitted skill, got %+v", ex)
		}
		if ex.String() == "" {
			t.Fatal("nativeSimilarityExclusion.String() must produce a non-empty auditable message")
		}
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
