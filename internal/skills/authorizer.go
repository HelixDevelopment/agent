package skills

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ToolAuditEntry records one skill-tool-call authorization decision
// (HXC-159 T-P6.03, RS-14). Every decision is recorded — allowed AND
// refused — so the audit trail is complete, never opt-in.
type ToolAuditEntry struct {
	SkillName string    `json:"skill_name"`
	ToolName  string    `json:"tool_name"`
	Allowed   bool      `json:"allowed"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

// SkillAuthorizer implements services.ToolCallAuthorizer by mapping a
// skill's declared AllowedTools onto tool-call permission — reusing this
// package's own ParseAllowedTools (T-P4.05's declared-capability model)
// verbatim, per T-P6.03.3's direction that the two must never diverge into
// a second policy language.
//
// Fail-closed by construction (T-P6.03 / RS-14, mirroring T-P4.05.3's
// "refuses rather than warns" precedent):
//   - actorID that does not resolve to a known skill: REFUSED. This
//     authorizer only ever gates SKILL-attributed tool calls; an actorID
//     it cannot resolve is not silently trusted.
//   - a skill whose AllowedTools is non-empty and does not name toolName:
//     REFUSED — the case RS-14 names explicitly ("a skill declaring Read
//     that attempts Write is refused").
//   - a skill with an EMPTY AllowedTools: ALLOWED. This is a deliberate,
//     documented choice (T-P6.03.3), not an oversight: the upstream
//     SKILL.md convention treats an omitted allowed-tools field as "no
//     declared restriction", and every existing skill in the current
//     corpus predates this enforcement — an implicit deny-all default
//     would refuse all of them the moment this landed. Enforcement fires
//     exactly when a skill DID declare a restriction.
type SkillAuthorizer struct {
	service *Service

	mu    sync.Mutex
	audit []ToolAuditEntry
}

// NewSkillAuthorizer constructs a SkillAuthorizer resolving actor IDs
// against service's registered skills.
func NewSkillAuthorizer(service *Service) *SkillAuthorizer {
	return &SkillAuthorizer{service: service}
}

// Authorize implements services.ToolCallAuthorizer.
func (a *SkillAuthorizer) Authorize(actorID, toolName string) (allowed bool, reason string) {
	skill, ok := a.service.GetSkill(actorID)
	switch {
	case !ok:
		allowed = false
		reason = fmt.Sprintf("actor %q is not a registered skill", actorID)
	case skill.AllowedTools == "":
		allowed = true
	default:
		allowed = skillDeclaresTool(skill, toolName)
		if !allowed {
			reason = fmt.Sprintf("skill %q declares AllowedTools=%q, which does not permit tool %q",
				skill.Name, skill.AllowedTools, toolName)
		}
	}

	entry := ToolAuditEntry{
		SkillName: actorID,
		ToolName:  toolName,
		Allowed:   allowed,
		Reason:    reason,
		At:        time.Now(),
	}
	a.mu.Lock()
	a.audit = append(a.audit, entry)
	a.mu.Unlock()

	return allowed, reason
}

// AuditLog returns a copy of every authorization decision recorded so far
// (RS-14: "the refusal is audited").
func (a *SkillAuthorizer) AuditLog() []ToolAuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ToolAuditEntry, len(a.audit))
	copy(out, a.audit)
	return out
}

// skillDeclaresTool reports whether skill's AllowedTools names toolName
// (case-insensitive — SKILL.md front-matter and tool names are both
// authored by hand and should not silently diverge on case). Uses
// ParseAllowedTools, the SAME parser T-P4.05 built for the declared-
// capability model, per T-P6.03.3.
func skillDeclaresTool(skill *Skill, toolName string) bool {
	for _, t := range ParseAllowedTools(skill.AllowedTools) {
		if strings.EqualFold(t.Name, toolName) {
			return true
		}
	}
	return false
}
