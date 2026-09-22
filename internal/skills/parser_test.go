package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleSkillMD = `---
name: "test-skill"
description: |
  Test skill for unit testing.
  Triggers on: "test trigger", "another trigger"
  Use when testing skill functionality.
allowed-tools: "Read, Write, Edit, Bash(cmd:*)"
version: 1.0.0
license: MIT
author: "Test Author <test@example.com>"
---

# Test Skill

## Overview

This is a test skill for unit testing purposes.

## When to Use

This skill activates automatically when you:
- Mention "test trigger" in your request
- Ask about testing patterns

## Instructions

1. Step one
2. Step two
3. Step three

## Examples

**Example: Basic Usage**
Request: "Help me with test trigger"
Result: Returns test results

**Example: Advanced Usage**
Request: "Run another trigger operation"
Result: Returns advanced results

## Prerequisites

- Go 1.21 or later
- Test framework installed

## Output

- Test results
- Coverage reports

## Error Handling

| Error | Cause | Solution |
|-------|-------|----------|
| Test failed | Bad input | Fix input |
| Timeout | Slow test | Increase timeout |

## Resources

- Go testing documentation
- Testify framework

## Related Skills

Part of the **Testing** skill category.
Tags: testing, go, unit-tests
`

func TestParser_Parse(t *testing.T) {
	parser := NewParser()

	skill, err := parser.Parse(sampleSkillMD, "/test/path/SKILL.md")
	require.NoError(t, err)
	assert.NotNil(t, skill)

	// Check frontmatter parsing
	assert.Equal(t, "test-skill", skill.Name)
	assert.Contains(t, skill.Description, "Test skill for unit testing")
	assert.Equal(t, "Read, Write, Edit, Bash(cmd:*)", skill.AllowedTools)
	assert.Equal(t, "1.0.0", skill.Version)
	assert.Equal(t, "MIT", skill.License)
	assert.Equal(t, "Test Author <test@example.com>", skill.Author)

	// Check trigger extraction
	assert.Contains(t, skill.TriggerPhrases, "test trigger")
	assert.Contains(t, skill.TriggerPhrases, "another trigger")

	// Check content sections
	assert.Contains(t, skill.Overview, "test skill for unit testing")
	assert.Contains(t, skill.WhenToUse, "test trigger")
	assert.Contains(t, skill.Instructions, "Step one")

	// Check examples
	assert.Len(t, skill.Examples, 2)
	if len(skill.Examples) >= 2 {
		assert.Equal(t, "Basic Usage", skill.Examples[0].Title)
		assert.Equal(t, "Advanced Usage", skill.Examples[1].Title)
	}

	// Check prerequisites
	assert.Len(t, skill.Prerequisites, 2)
	assert.Contains(t, skill.Prerequisites, "Go 1.21 or later")

	// Check outputs
	assert.Len(t, skill.Outputs, 2)
	assert.Contains(t, skill.Outputs, "Test results")

	// Check error handling
	assert.Len(t, skill.ErrorHandling, 2)
	if len(skill.ErrorHandling) >= 1 {
		assert.Equal(t, "Test failed", skill.ErrorHandling[0].Error)
		assert.Equal(t, "Bad input", skill.ErrorHandling[0].Cause)
	}

	// Check tags
	assert.Contains(t, skill.Tags, "testing")
	assert.Contains(t, skill.Tags, "go")

	// Check metadata
	assert.Equal(t, "/test/path/SKILL.md", skill.FilePath)
	assert.False(t, skill.LoadedAt.IsZero())
}

func TestParser_SplitFrontmatter(t *testing.T) {
	parser := NewParser()

	tests := []struct {
		name            string
		content         string
		wantFrontmatter string
		wantBody        string
		wantErr         bool
	}{
		{
			name:            "valid frontmatter",
			content:         "---\nname: test\n---\n# Content",
			wantFrontmatter: "name: test",
			wantBody:        "# Content",
			wantErr:         false,
		},
		{
			name:            "no frontmatter",
			content:         "# Just content",
			wantFrontmatter: "",
			wantBody:        "# Just content",
			wantErr:         false,
		},
		{
			name:    "unterminated frontmatter",
			content: "---\nname: test\n# No closing",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, err := parser.splitFrontmatter(tt.content)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantFrontmatter, fm)
			assert.Contains(t, body, tt.wantBody)
		})
	}
}

func TestParser_ExtractTriggers(t *testing.T) {
	parser := NewParser()

	tests := []struct {
		name        string
		description string
		want        []string
	}{
		{
			name:        "explicit triggers",
			description: `Triggers on: "foo", "bar", "baz"`,
			want:        []string{"foo", "bar", "baz"},
		},
		{
			name:        "trigger on format",
			description: `Trigger on: hello world`,
			want:        []string{"hello world"},
		},
		{
			name:        "quoted phrases",
			description: `Use "docker compose" for containers and "kubernetes" for orchestration`,
			want:        []string{"docker compose", "kubernetes"},
		},
		{
			name:        "no triggers",
			description: `Just a simple description`,
			want:        []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			triggers := parser.extractTriggers(tt.description)
			for _, want := range tt.want {
				assert.Contains(t, triggers, want)
			}
		})
	}
}

func TestParser_ExtractCategory(t *testing.T) {
	parser := NewParser()

	tests := []struct {
		path string
		want string
	}{
		{
			path: "/skills/01-devops-basics/docker-compose-creator/SKILL.md",
			want: "01-devops-basics",
		},
		{
			path: "/some/path/skills/13-aws-skills/lambda-config/SKILL.md",
			want: "13-aws-skills",
		},
		{
			path: "/no-skills-dir/other/SKILL.md",
			want: "uncategorized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := parser.extractCategory(tt.path)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParser_ParseDirectory(t *testing.T) {
	// Create temporary directory with test skills
	tempDir := t.TempDir()

	// Create skill directory structure
	skillDir := filepath.Join(tempDir, "skills", "01-test-category", "test-skill")
	err := os.MkdirAll(skillDir, 0755)
	require.NoError(t, err)

	// Write skill file
	skillFile := filepath.Join(skillDir, "SKILL.md")
	err = os.WriteFile(skillFile, []byte(sampleSkillMD), 0644)
	require.NoError(t, err)

	// Parse directory
	parser := NewParser()
	skills, failures, err := parser.ParseDirectory(tempDir)
	require.NoError(t, err)
	assert.Len(t, skills, 1)
	assert.Len(t, failures, 0)

	if len(skills) > 0 {
		assert.Equal(t, "test-skill", skills[0].Name)
		assert.Equal(t, "01-test-category", skills[0].Category)
	}
}

func TestParser_ParseList(t *testing.T) {
	parser := NewParser()

	content := `
- Item one
- Item two
* Item three
- Item four
Regular text
`

	items := parser.parseList(content)
	assert.Len(t, items, 4)
	assert.Contains(t, items, "Item one")
	assert.Contains(t, items, "Item two")
	assert.Contains(t, items, "Item three")
	assert.Contains(t, items, "Item four")
}

func TestParser_ParseErrorTable(t *testing.T) {
	parser := NewParser()

	content := `
| Error | Cause | Solution |
|-------|-------|----------|
| Not found | Missing file | Create file |
| Permission denied | Bad perms | Fix perms |
`

	errors := parser.parseErrorTable(content)
	assert.Len(t, errors, 2)

	if len(errors) >= 2 {
		assert.Equal(t, "Not found", errors[0].Error)
		assert.Equal(t, "Missing file", errors[0].Cause)
		assert.Equal(t, "Create file", errors[0].Solution)
	}
}
