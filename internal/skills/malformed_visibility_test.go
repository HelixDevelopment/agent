package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HXC-159 T-P6.04 — Surface malformed-skill failures.
//
// RED baseline (§11.4.115): before this task's fix, ParseDirectory silently
// swallowed a malformed SKILL.md — it logged at Debug ("optional skills that
// fail to parse should not clutter logs") and returned ONLY the skills that
// parsed cleanly, with zero operator-visible signal that anything was
// dropped. With a 1174-file production corpus a malformed skill disappears
// without a trace. This file's test asserted that defect (silent vanish)
// against the two-return-value ParseDirectory signature; it now asserts the
// fixed, three-return-value signature that makes every failure visible.
func TestParser_ParseDirectory_MalformedSkillIsSurfacedNotSilentlyDropped(t *testing.T) {
	tempDir := t.TempDir()

	// One well-formed skill.
	goodDir := filepath.Join(tempDir, "skills", "01-good", "good-skill")
	require.NoError(t, os.MkdirAll(goodDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(goodDir, "SKILL.md"), []byte(sampleSkillMD), 0644))

	// One deliberately malformed skill: frontmatter delimiters present but
	// the YAML body is not a mapping (a bare scalar), so yaml.Unmarshal into
	// *Skill fails exactly as production malformed skills have been
	// observed to fail (§11.4.146 STEP 1 characterisation).
	badDir := filepath.Join(tempDir, "skills", "02-bad", "malformed-skill")
	require.NoError(t, os.MkdirAll(badDir, 0755))
	malformed := "---\njust-a-scalar-not-a-mapping\n---\n\n# Malformed\n"
	require.NoError(t, os.WriteFile(filepath.Join(badDir, "SKILL.md"), []byte(malformed), 0644))

	parser := NewParser()
	got, failures, err := parser.ParseDirectory(tempDir)
	require.NoError(t, err, "ParseDirectory itself must not error for a directory-level walk failure — per-file failures are reported, not fatal (T-P6.04.3 fail-loud-and-continue)")

	// The good skill still loads — a malformed sibling must not take down
	// the whole directory (T-P6.04.3 documented choice: fail-loud, not
	// fail-closed, for this consumer's live-registry use case). Name comes
	// from sampleSkillMD's own frontmatter ("test-skill"), not the
	// directory basename — the parser prefers the declared name.
	require.Len(t, got, 1)
	assert.Equal(t, "test-skill", got[0].Name)

	// The malformed skill is NOT silently dropped: it is reported, with the
	// offending path and a non-empty reason.
	require.Len(t, failures, 1, "malformed skill must be surfaced in the load report, never silently skipped")
	assert.Contains(t, failures[0].Path, "malformed-skill")
	assert.NotEmpty(t, failures[0].Error, "the failure must carry an operator-visible reason")
}

// TestParser_ParseDirectory_NoFailures_EmptyFailureSlice pins the
// non-regression shape: a directory with only well-formed skills reports
// zero failures (not nil-vs-empty ambiguity in either direction — either is
// acceptable to callers, but the count must be exactly zero).
func TestParser_ParseDirectory_NoFailures_EmptyFailureSlice(t *testing.T) {
	tempDir := t.TempDir()
	skillDir := filepath.Join(tempDir, "skills", "01-test-category", "test-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(sampleSkillMD), 0644))

	parser := NewParser()
	got, failures, err := parser.ParseDirectory(tempDir)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Len(t, failures, 0)
}
