package skills

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParser_ParseDirectory_FollowsSymlinkedSkillDirectories reproduces
// HXC-159 T-P9.01 round-4 finding F-23: filepath.Walk (the pre-fix
// implementation of ParseDirectory) does not follow directory symlinks, so
// a source directory shaped like the registrar's real output —
// `.claude/skills/<name>` as a symlink to `constitution/skills/<name>`
// (scripts/register_skills.sh, T-P1.04) — parsed as EMPTY with no error:
// the same §11.4.201(6) false-null class F-03 already closed in the
// sibling `pkg/skills` loader (submodules/skills/pkg/skills/loader.go).
//
// This fixture mirrors that exact shape: a real skill directory holding a
// real SKILL.md, plus a SEPARATE "external" directory containing only a
// symlink to it — the shape ExternalSkillSource.Load() sees when pointed
// at .claude/skills/.
func TestParser_ParseDirectory_FollowsSymlinkedSkillDirectories(t *testing.T) {
	root := t.TempDir()

	// The real skill lives under a directory that is NOT itself scanned
	// directly by the test (mirrors constitution/skills/<name>).
	realParent := filepath.Join(root, "real_skills")
	realSkillDir := filepath.Join(realParent, "symlinked-skill")
	require.NoError(t, os.MkdirAll(realSkillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realSkillDir, "SKILL.md"), []byte(sampleSkillMD), 0o644))

	// The "external" tier directory (mirrors .claude/skills/) contains
	// ONLY a symlink to the real skill directory above — exactly what
	// scripts/register_skills.sh provisions.
	externalDir := filepath.Join(root, "external_skills")
	require.NoError(t, os.MkdirAll(externalDir, 0o755))
	require.NoError(t, os.Symlink(realSkillDir, filepath.Join(externalDir, "symlinked-skill")))

	parser := NewParser()
	skills, failures, err := parser.ParseDirectory(externalDir)
	require.NoError(t, err)
	require.Empty(t, failures, "the symlinked SKILL.md should parse cleanly, not fail")

	// THE ASSERTION: a symlink-aware ParseDirectory MUST discover the
	// skill through the symlinked directory. Pre-fix (filepath.Walk), this
	// returns 0 skills with no error at all — the silent-empty bug.
	require.Len(t, skills, 1, "ParseDirectory must follow symlinked skill directories (F-23) — got %d skills, want 1", len(skills))
	if len(skills) == 1 {
		assert.Equal(t, "test-skill", skills[0].Name)
	}
}

// TestParser_ParseDirectory_SymlinkCycleTerminates guards the fix's cycle
// safety: a symlink that (directly) points back at its own parent must
// not hang or infinitely recurse the walk.
func TestParser_ParseDirectory_SymlinkCycleTerminates(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "cyclic")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(sampleSkillMD), 0o644))
	// self-referential symlink inside the directory it lives in
	require.NoError(t, os.Symlink(skillDir, filepath.Join(skillDir, "self")))

	parser := NewParser()
	done := make(chan struct{})
	var skills []*Skill
	var err error
	go func() {
		skills, _, err = parser.ParseDirectory(root)
		close(done)
	}()

	select {
	case <-done:
		require.NoError(t, err)
		require.Len(t, skills, 1)
	case <-time.After(10 * time.Second):
		t.Fatal("ParseDirectory did not terminate on a symlink cycle within the timeout")
	}
}
