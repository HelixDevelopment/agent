package skills

import (
	"fmt"
	"log"
	"sort"
	"strings"
)

// HXC-159 T-P9.01 finding F-01 remediation (HelixAgent side).
//
// The review found that neither consumer imported github.com/HelixDevelopment/
// skills: the P4 activation policy (allowlist, activation ceiling,
// description-similarity gate, C5 sandbox precondition, capability-based
// refusal) existed only inside that library. HelixAgent's own
// ExternalSkillSource (external_source.go) IMPLIED activation by listing
// whatever SKILL.md files ParseDirectory found — every discovered skill was
// registered into services.ToolRegistry AND skills.Service unconditionally,
// with none of the above policy ever consulted.
//
// D-4 (spec.md decision) forbids HelixAgent from Go-importing
// github.com/HelixDevelopment/skills — upstream's own helix-deps.yaml
// already declares HelixAgent as a dependency OF the skills library (for
// LLM/embeddings), so the reverse edge would create a forbidden two-way
// cycle (CONST-051(C)). tests/compliance/module_graph_edge_test.go is the
// mechanical gate that enforces the module graph never grows that edge.
//
// This file is therefore a NATIVE, non-importing reimplementation of the
// SAME policy engine pkg/skills ships — faithfully ported from
// pkg/skills/activate.go + similarity.go + sandbox_gate.go (constants and
// algorithm cited verbatim below) so both consumers enforce the identical
// rules without sharing code across the forbidden edge. Capability-based
// refusal is NOT reimplemented here — this package's pre-existing
// SkillAuthorizer (authorizer.go) already provides it via each skill's
// AllowedTools declaration, and per the "never duplicate a policy
// mechanism" discipline (mirroring pkg/skills' own §11.4.227
// extend-don't-duplicate comment on VendoredTierEnforced) it is reused
// as-is, not re-ported.

// nativeDefaultCeiling mirrors pkg/skills.DefaultCeiling (T-P2.03 measured
// output: the shadowing curve was flat at N=20, no knee — provisional
// ceiling, not a guess). Ported verbatim; changing it here without a
// matching re-measurement on pkg/skills' side would silently diverge the
// two consumers' policies.
const nativeDefaultCeiling = 20

// nativeSimilarityThreshold mirrors pkg/skills.SimilarityThreshold
// (T-P2.04 calibration: the Jaccard-token metric is NON-SEPARABLE on the
// calibration corpus, so the threshold falls back to
// min(known_confusable)=0.0256 with the false-positive risk explicitly
// accepted — see pkg/skills/similarity.go for the full calibration note).
const nativeSimilarityThreshold = 0.0256

// nativeStopwords is pkg/skills' stopwords set, copied verbatim (T-P2.04
// calibration script parity) — see pkg/skills/similarity.go's comment: "The
// Go metric MUST use this set verbatim — any drift silently re-calibrates
// the threshold." Any change here MUST be mirrored on the pkg/skills side
// and vice versa, since the two are no longer the same compiled code.
var nativeStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true,
	"of": true, "to": true, "in": true, "on": true, "for": true,
	"with": true, "as": true, "by": true, "at": true, "from": true,
	"is": true, "are": true, "was": true, "were": true, "be": true,
	"been": true, "it": true, "its": true, "this": true, "that": true,
	"these": true, "those": true, "you": true, "your": true, "we": true,
	"our": true, "they": true, "their": true, "he": true, "she": true,
	"him": true, "her": true, "them": true, "his": true, "hers": true,
	"ours": true, "theirs": true, "my": true, "mine": true, "use": true,
	"using": true, "used": true, "when": true, "whenever": true,
	"where": true, "which": true, "who": true, "whom": true,
	"whose": true, "what": true, "how": true, "why": true, "not": true,
	"no": true, "nor": true, "if": true, "then": true, "than": true,
	"so": true, "such": true, "can": true, "could": true, "should": true,
	"would": true, "may": true, "might": true, "must": true, "will": true,
	"shall": true, "do": true, "does": true, "did": true, "done": true,
	"have": true, "has": true, "had": true, "having": true, "any": true,
	"all": true, "each": true, "every": true, "both": true, "either": true,
	"neither": true, "one": true, "two": true, "more": true, "most": true,
	"other": true, "some": true, "over": true, "under": true, "into": true,
	"out": true, "about": true, "above": true, "below": true,
	"between": true, "through": true, "during": true, "before": true,
	"after": true, "per": true, "via": true, "also": true, "only": true,
	"just": true, "very": true, "too": true, "vs": true, "etc": true,
	"including": true, "include": true, "includes": true, "within": true,
	"without": true, "name": true, "skill": true, "system": true,
}

// nativeTokens is pkg/skills.Tokens, ported verbatim.
func nativeTokens(text string) map[string]bool {
	lowered := strings.ToLower(text)
	set := map[string]bool{}
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		tok := lowered[start:end]
		if len(tok) > 1 && !nativeStopwords[tok] {
			set[tok] = true
		}
		start = -1
	}
	for i := 0; i < len(lowered); i++ {
		c := lowered[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(lowered))
	return set
}

// nativeJaccard is pkg/skills.Jaccard, ported verbatim (both-empty scores
// 1.0, one-empty scores 0.0 — calibration-script parity).
func nativeJaccard(a, b string) float64 {
	ta, tb := nativeTokens(a), nativeTokens(b)
	if len(ta) == 0 && len(tb) == 0 {
		return 1.0
	}
	if len(ta) == 0 || len(tb) == 0 {
		return 0.0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	return float64(inter) / float64(union)
}

// ExternalSourceTrust classifies an ExternalSkillSource for the C5 sandbox
// precondition (mirrors pkg/skills.TrustTier — a native, non-importing
// port of the same three-value closed set).
type ExternalSourceTrust int

const (
	// TrustVendored matches this consumer's pre-existing self-report in
	// internal/skillconformance/report.go's trustForTier("external") — the
	// external tier's primary provisioner (scripts/register_skills.sh)
	// links constitution-owned governance skills by reference, not
	// arbitrary untrusted third-party code. This is the default trust
	// level for ExternalSkillSource.
	TrustVendored ExternalSourceTrust = iota
	// TrustThirdParty is for sources whose content is genuinely untrusted
	// (a caller that discovers a source carrying arbitrary third-party
	// code sets this explicitly) — the C5 sandbox precondition refuses
	// activation of a TrustThirdParty source until a real sandbox executor
	// is wired.
	TrustThirdParty
)

// nativeCeilingExceededError mirrors pkg/skills.CeilingExceededError.
type nativeCeilingExceededError struct {
	Ceiling  int
	Count    int
	Offender string
}

func (e *nativeCeilingExceededError) Error() string {
	return fmt.Sprintf("skills: external source active set %d exceeds ceiling %d — refusing; offender %q (drop it or raise the ceiling with a re-measured curve, T-P2.03)",
		e.Count, e.Ceiling, e.Offender)
}

// nativeSimilarityConflictError mirrors pkg/skills.SimilarityConflictError.
type nativeSimilarityConflictError struct {
	A, B      string
	Score     float64
	Threshold float64
}

func (e *nativeSimilarityConflictError) Error() string {
	return fmt.Sprintf("skills: external source active pair %q || %q scores %.4f above similarity threshold %.4f (T-P2.04 NON-SEPARABLE fallback, fp risk accepted) — refusing; drop or rename one description",
		e.A, e.B, e.Score, e.Threshold)
}

// nativeThirdPartyBlockedError mirrors pkg/skills.ThirdPartyActivationBlockedError.
type nativeThirdPartyBlockedError struct {
	Offenders []string
}

func (e *nativeThirdPartyBlockedError) Error() string {
	return fmt.Sprintf("skills: refusing external-source activation — %d third-party-tier skill(s) [%s] cannot be activated while the sandbox precondition holds (C5: no real sandbox executor wired); set ExternalSkillSource.SandboxReady=true only once one is",
		len(e.Offenders), strings.Join(e.Offenders, ", "))
}

// qualifiedName reproduces pkg/skills' Skill.Qualified() naming convention
// (<source>.<name>) for this package's own *Skill type, so the ported
// ceiling/similarity refusal messages name offenders in the same shape.
func qualifiedName(sourceName string, s *Skill) string {
	return sourceName + "." + s.Name
}

// externalPolicyGate is the native, non-importing port of
// pkg/skills.Registry.Activate's enforcement order (ceiling, then
// similarity, then the C5 sandbox precondition — capability-based refusal
// is SkillAuthorizer's separate, pre-existing job and is not part of this
// gate). discovered is every skill ParseDirectory found in the source
// directory for THIS Load() call; the "allowlist" is implicitly every
// discovered skill's qualified name (mirroring this same fix's helix_code
// counterpart, internal/agent/skill_external_policy.go, which makes the
// identical simplification for the same reason: an external tier has no
// separate allowlist file of its own, the directory listing itself is the
// declared set of candidates for policy admission).
//
// On success, active is every discovered skill (policy admits the whole
// tier). On a policy refusal (ceiling / similarity / sandbox), active is
// nil and refusal names exactly which rule fired and why — the caller
// (ExternalSkillSource.Load) treats this as "admit zero skills from this
// source this cycle", never a partial admission.
func externalPolicyGate(sourceName string, discovered []*Skill, trust ExternalSourceTrust, sandboxReady bool) (active []*Skill, refusal error) {
	if len(discovered) == 0 {
		return nil, nil
	}

	ordered := make([]*Skill, len(discovered))
	copy(ordered, discovered)
	sort.Slice(ordered, func(i, j int) bool {
		return qualifiedName(sourceName, ordered[i]) < qualifiedName(sourceName, ordered[j])
	})

	if len(ordered) > nativeDefaultCeiling {
		offender := qualifiedName(sourceName, ordered[nativeDefaultCeiling])
		return nil, &nativeCeilingExceededError{
			Ceiling:  nativeDefaultCeiling,
			Count:    len(ordered),
			Offender: offender,
		}
	}

	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			score := nativeJaccard(ordered[i].Description, ordered[j].Description)
			if score > nativeSimilarityThreshold {
				return nil, &nativeSimilarityConflictError{
					A:         qualifiedName(sourceName, ordered[i]),
					B:         qualifiedName(sourceName, ordered[j]),
					Score:     score,
					Threshold: nativeSimilarityThreshold,
				}
			}
		}
	}

	if trust == TrustThirdParty && !sandboxReady {
		offenders := make([]string, len(ordered))
		for i, s := range ordered {
			offenders[i] = qualifiedName(sourceName, s)
		}
		return nil, &nativeThirdPartyBlockedError{Offenders: offenders}
	}

	return ordered, nil
}

// logPolicyRefusal is the single place ExternalSkillSource.Load logs a
// policy refusal — kept as a named function so a future caller wanting a
// structured logger (this package already has one, via Service.SetLogger,
// for its OTHER components) can swap the implementation in one place
// without touching externalPolicyGate's pure decision logic.
func logPolicyRefusal(sourceName string, refusal error) {
	log.Printf("⚠️  skills: external source %q refused by policy: %v", sourceName, refusal)
}
