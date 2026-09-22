package skills

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Pre-compiled regexes for skill parsing
var (
	skillQuotedRegex   = regexp.MustCompile(`"([^"]+)"`)
	skillExampleRegex  = regexp.MustCompile(`\*\*Example:?\s*([^*]*)\*\*\s*\n([^*]+)`)
	skillCategoryRegex = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	skillTagRegex      = regexp.MustCompile(`Tags?:\s*([^\n]+)`)
)

// Parser handles parsing of SKILL.md files.
type Parser struct {
	triggerPattern *regexp.Regexp
}

// NewParser creates a new SKILL.md parser.
func NewParser() *Parser {
	return &Parser{
		// Pattern to extract trigger phrases from description
		triggerPattern: regexp.MustCompile(`(?i)triggers?\s+on:?\s*([^.]+)`),
	}
}

// ParseFile parses a single SKILL.md file.
func (p *Parser) ParseFile(filePath string) (*Skill, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read skill file: %w", err)
	}

	return p.Parse(string(content), filePath)
}

// Parse parses SKILL.md content string.
func (p *Parser) Parse(content string, filePath string) (*Skill, error) {
	skill := &Skill{
		FilePath:  filePath,
		LoadedAt:  time.Now(),
		UpdatedAt: time.Now(),
	}

	// Split frontmatter and content
	frontmatter, body, err := p.splitFrontmatter(content)
	if err != nil {
		return nil, fmt.Errorf("failed to split frontmatter: %w", err)
	}

	// Parse YAML frontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), skill); err != nil {
		return nil, fmt.Errorf("failed to parse frontmatter: %w", err)
	}

	// Store raw content
	skill.RawContent = body

	// Extract trigger phrases from description
	skill.TriggerPhrases = p.extractTriggers(skill.Description)

	// Parse content sections
	p.parseContentSections(skill, body)

	// Extract category from file path if not set in frontmatter
	if skill.Category == "" {
		skill.Category = p.extractCategory(filePath)
	}

	// Extract tags from related skills section
	skill.Tags = p.extractTags(body)

	return skill, nil
}

// splitFrontmatter separates YAML frontmatter from markdown content.
func (p *Parser) splitFrontmatter(content string) (frontmatter, body string, err error) {
	lines := strings.Split(content, "\n")

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", content, nil // No frontmatter
	}

	inFrontmatter := false
	frontmatterEnd := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			} else {
				frontmatterEnd = i
				break
			}
		}
	}

	if frontmatterEnd == -1 {
		return "", content, fmt.Errorf("unterminated frontmatter")
	}

	frontmatter = strings.Join(lines[1:frontmatterEnd], "\n")
	body = strings.Join(lines[frontmatterEnd+1:], "\n")

	return frontmatter, body, nil
}

// extractTriggers extracts trigger phrases from the description.
func (p *Parser) extractTriggers(description string) []string {
	triggers := make([]string, 0)

	// Find explicit trigger phrases
	matches := p.triggerPattern.FindStringSubmatch(description)
	if len(matches) > 1 {
		// Split by comma and clean up
		parts := strings.Split(matches[1], ",")
		for _, part := range parts {
			trigger := strings.TrimSpace(part)
			trigger = strings.Trim(trigger, `"'`)
			if trigger != "" {
				triggers = append(triggers, strings.ToLower(trigger))
			}
		}
	}

	quotedMatches := skillQuotedRegex.FindAllStringSubmatch(description, -1)
	for _, match := range quotedMatches {
		if len(match) > 1 {
			trigger := strings.ToLower(strings.TrimSpace(match[1]))
			if trigger != "" && !contains(triggers, trigger) {
				triggers = append(triggers, trigger)
			}
		}
	}

	return triggers
}

// parseContentSections extracts structured content from markdown.
func (p *Parser) parseContentSections(skill *Skill, body string) {
	scanner := bufio.NewScanner(strings.NewReader(body))
	currentSection := ""
	currentContent := strings.Builder{}

	for scanner.Scan() {
		line := scanner.Text()

		// Check for section headers
		if strings.HasPrefix(line, "## ") {
			// Save previous section
			p.saveSection(skill, currentSection, currentContent.String())
			currentSection = strings.TrimPrefix(line, "## ")
			currentContent.Reset()
		} else {
			currentContent.WriteString(line)
			currentContent.WriteString("\n")
		}
	}

	// Save last section
	p.saveSection(skill, currentSection, currentContent.String())
}

// saveSection saves a parsed section to the skill.
func (p *Parser) saveSection(skill *Skill, section, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	section = strings.ToLower(section)

	switch {
	case strings.Contains(section, "overview"):
		skill.Overview = content
	case strings.Contains(section, "when to use"):
		skill.WhenToUse = content
	case strings.Contains(section, "instruction"):
		skill.Instructions = content
	case strings.Contains(section, "example"):
		skill.Examples = p.parseExamples(content)
	case strings.Contains(section, "prerequisite"):
		skill.Prerequisites = p.parseList(content)
	case strings.Contains(section, "output"):
		skill.Outputs = p.parseList(content)
	case strings.Contains(section, "error"):
		skill.ErrorHandling = p.parseErrorTable(content)
	case strings.Contains(section, "resource"):
		skill.Resources = p.parseList(content)
	case strings.Contains(section, "related"):
		skill.RelatedSkills = p.parseRelated(content)
	}
}

// parseExamples parses example blocks from content.
func (p *Parser) parseExamples(content string) []SkillExample {
	examples := make([]SkillExample, 0)

	matches := skillExampleRegex.FindAllStringSubmatch(content, -1)

	for _, match := range matches {
		if len(match) > 2 {
			example := SkillExample{
				Title: strings.TrimSpace(match[1]),
			}

			// Parse Request/Result
			lines := strings.Split(match[2], "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "Request:") {
					example.Request = strings.TrimPrefix(line, "Request:")
					example.Request = strings.TrimSpace(example.Request)
				} else if strings.HasPrefix(line, "Result:") {
					// Result might span multiple lines
					result := strings.TrimPrefix(line, "Result:")
					for j := i + 1; j < len(lines); j++ {
						if strings.TrimSpace(lines[j]) == "" {
							break
						}
						result += " " + strings.TrimSpace(lines[j])
					}
					example.Result = strings.TrimSpace(result)
				}
			}

			examples = append(examples, example)
		}
	}

	return examples
}

// parseList parses a markdown list into string slice.
func (p *Parser) parseList(content string) []string {
	items := make([]string, 0)
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			item := strings.TrimPrefix(line, "- ")
			items = append(items, strings.TrimSpace(item))
		} else if strings.HasPrefix(line, "* ") {
			item := strings.TrimPrefix(line, "* ")
			items = append(items, strings.TrimSpace(item))
		}
	}

	return items
}

// parseErrorTable parses the error handling table.
func (p *Parser) parseErrorTable(content string) []SkillError {
	errors := make([]SkillError, 0)

	// Simple table parsing
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "|") && !strings.Contains(line, "---") && !strings.Contains(strings.ToLower(line), "error") {
			parts := strings.Split(line, "|")
			if len(parts) >= 4 {
				se := SkillError{
					Error:    strings.TrimSpace(parts[1]),
					Cause:    strings.TrimSpace(parts[2]),
					Solution: strings.TrimSpace(parts[3]),
				}
				if se.Error != "" {
					errors = append(errors, se)
				}
			}
		}
	}

	return errors
}

// parseRelated parses related skills section.
func (p *Parser) parseRelated(content string) []string {
	related := make([]string, 0)

	matches := skillCategoryRegex.FindAllStringSubmatch(content, -1)
	for _, match := range matches {
		if len(match) > 1 {
			related = append(related, strings.TrimSpace(match[1]))
		}
	}

	return related
}

// extractCategory extracts category from file path.
func (p *Parser) extractCategory(filePath string) string {
	// Path pattern: skills/01-devops-basics/skill-name/SKILL.md
	dir := filepath.Dir(filePath)
	parts := strings.Split(dir, string(filepath.Separator))

	for i, part := range parts {
		if part == "skills" && i+1 < len(parts) {
			return parts[i+1]
		}
	}

	return "uncategorized"
}

// extractTags extracts tags from content.
func (p *Parser) extractTags(content string) []string {
	tags := make([]string, 0)

	matches := skillTagRegex.FindStringSubmatch(content)
	if len(matches) > 1 {
		parts := strings.Split(matches[1], ",")
		for _, part := range parts {
			tag := strings.TrimSpace(part)
			if tag != "" {
				tags = append(tags, tag)
			}
		}
	}

	return tags
}

// ParseFailure records one SKILL.md file that ParseDirectory encountered but
// could not parse (HXC-159 T-P6.04). Path is the exact file that failed;
// Error is the operator-visible reason (never swallowed).
type ParseFailure struct {
	Path  string
	Error string
}

// ParseDirectory recursively parses all SKILL.md files in a directory.
//
// T-P6.04 fail-loud-and-continue (documented choice, NOT fail-closed): a
// malformed SKILL.md is surfaced in the returned []ParseFailure — logged at
// Warn (raised from the previous Debug, which is why the defect this fixes
// was invisible with a 1174-file production corpus: "optional skills that
// fail to parse should not clutter logs" silently dropped every malformed
// skill with zero operator signal) — but does NOT abort the whole walk.
// Rationale: this parser backs a LIVE HTTP-facing skill registry
// (Registry.Load / LoadFromPath); refusing every skill because one file is
// malformed would take down a working registry over an isolated authoring
// mistake, which is a worse operational failure mode for this consumer than
// for a one-shot batch loader (contrast the upstream HelixSkills library's
// LoadSource, which IS fail-closed per-source — a deliberate difference
// recorded here, not an oversight: that loader seeds a fresh index from a
// pinned corpus where an aborted load is safely retryable before anything
// depends on it; this one refreshes a registry callers are already using).
func (p *Parser) ParseDirectory(dir string) ([]*Skill, []ParseFailure, error) {
	skills := make([]*Skill, 0)
	var failures []ParseFailure

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.ToUpper(info.Name()) == "SKILL.MD" {
			skill, parseErr := p.ParseFile(path)
			if parseErr != nil {
				// Raised from Debug to Warn (T-P6.04.2): a malformed skill
				// MUST be operator-visible, never a silent Debug-only entry
				// a live deployment never reads.
				logrus.WithError(parseErr).WithField("path", path).
					Warn("Skill file failed to parse — surfaced in load report, not silently skipped")
				failures = append(failures, ParseFailure{Path: path, Error: parseErr.Error()})
				return nil
			}
			skills = append(skills, skill)
		}

		return nil
	})

	if err != nil {
		return nil, nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	return skills, failures, nil
}

// contains checks if a slice contains a string.
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
