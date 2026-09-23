package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HXC-159 T-P9.01 finding F-16 (round-3 review fix).
//
// RED baseline (§11.4.115): a SKILL.md with NO YAML front-matter block at
// all parses "successfully" (splitFrontmatter treats a missing "---" as
// "no frontmatter, whole file is body"), yields Skill.Name == "", and is
// then dropped by Registry.registerSkillLocked's `if skill.Name == "" {
// return }` with ZERO operator-visible signal — never in GetAllSkills(),
// never in LoadFailures(). Measured live 2026-09-23 on the real
// constitution/skills corpus: 3 of 8 directories (media-validator,
// scheduled-work-queue, session-sync) vanished this way. That is exactly
// the silent-failure class T-P6.04 closed for the malformed-YAML path.
//
// Fix: ParseDirectory reports an un-registrable (nameless) skill as a
// ParseFailure — surfaced through the SAME T-P6.04 load report — instead of
// returning it as a discovered skill the registry will silently discard.
// Identity DERIVATION for nameless skills (option (a) in the ledger) is a
// separate design decision and is deliberately NOT made here.
func TestParser_ParseDirectory_FrontmatterlessSkillIsSurfacedNotSilentlyDropped(t *testing.T) {
	tempDir := t.TempDir()

	goodDir := filepath.Join(tempDir, "skills", "01-good", "good-skill")
	require.NoError(t, os.MkdirAll(goodDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(goodDir, "SKILL.md"), []byte(sampleSkillMD), 0644))

	// Bare H1, no front-matter block — the real shape of 3 constitution skills.
	bareDir := filepath.Join(tempDir, "skills", "02-bare", "media-validator")
	require.NoError(t, os.MkdirAll(bareDir, 0755))
	bare := "# Media Validator Skill\n\n## Purpose\n\nValidate a recording by reading its content.\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "SKILL.md"), []byte(bare), 0644))

	parser := NewParser()
	got, failures, err := parser.ParseDirectory(tempDir)
	require.NoError(t, err)

	require.Len(t, got, 1, "only the registrable skill may be returned as discovered")
	assert.Equal(t, "test-skill", got[0].Name)

	require.Len(t, failures, 1, "a nameless (front-matter-less) skill must be surfaced in the load report, never silently dropped")
	assert.Contains(t, failures[0].Path, "media-validator")
	assert.Contains(t, failures[0].Error, "name", "the reason must name the missing front-matter name")
}

// The same defect observed at the registry seam (the surface T-P6.04's
// LoadFailures() contract is about): after LoadFromPath over a directory
// holding a nameless skill, LoadFailures() MUST name it and GetAllSkills()
// MUST NOT contain a phantom.
func TestRegistry_LoadFromPath_FrontmatterlessSkillAppearsInLoadFailures(t *testing.T) {
	tempDir := t.TempDir()
	bareDir := filepath.Join(tempDir, "skills", "02-bare", "session-sync")
	require.NoError(t, os.MkdirAll(bareDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "SKILL.md"), []byte("# Session Sync Skill\n\nbody\n"), 0644))

	reg := NewRegistry(nil)
	require.NoError(t, reg.LoadFromPath(context.Background(), tempDir))

	assert.Len(t, reg.GetAll(), 0, "a nameless skill must not be registered under an empty key")
	failures := reg.LoadFailures()
	require.Len(t, failures, 1, "the dropped skill must be visible in LoadFailures() — silent drop is the F-16 defect")
	assert.Contains(t, failures[0].Path, "session-sync")
}
