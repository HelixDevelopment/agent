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
// (HXC-159 F-17 recalibration, 2026-09-23: the ORIGINAL T-P2.04
// calibration mixed the real co-activated corpus with corpus-2's
// unrelated postgres-mcp skills, which are TrustVendored and never in
// the default allowlist -- that mixing pulled the threshold down to a
// non-separable 0.0256 that then refused the real corpus on its own
// governance-skill descriptions. On the REAL co-activated corpus the
// metric IS separable: threshold=0.0709, fp_risk_accepted=false — see
// pkg/skills/similarity.go for the full calibration note and
// scripts/benchmark_skills/similarity.py for the per-pair justification.
// MUST be mirrored on the pkg/skills side and vice versa.
const nativeSimilarityThreshold = 0.0709

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

// nativeSimilarityExclusion mirrors pkg/skills.SimilarityExclusion (HXC-159
// F-17, 2026-09-23): the admission model moved from whole-tier fail-closed
// to per-skill (partial) admission. It is the per-skill audit record
// produced when the description-similarity gate excludes ONE member of a
// confusable pair — names the excluded skill, the already-admitted skill it
// conflicts with, the measured score, and the threshold — never a bare
// boolean and never an opaque partial list. MUST be kept in lockstep with
// pkg/skills.SimilarityExclusion per this file's mirroring mandate.
type nativeSimilarityExclusion struct {
	Excluded      string
	ConflictsWith string
	Score         float64
	Threshold     float64
}

func (e nativeSimilarityExclusion) String() string {
	return fmt.Sprintf("skills: external source %q excluded — scores %.4f above similarity threshold %.4f against already-admitted %q (see threshold.json for the current calibration); the alphabetically-later member of a confusable pair is excluded so the earlier one may still load",
		e.Excluded, e.Score, e.Threshold, e.ConflictsWith)
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
// per-skill similarity admission, then the C5 sandbox precondition —
// capability-based refusal is SkillAuthorizer's separate, pre-existing job
// and is not part of this gate). discovered is every skill ParseDirectory
// found in the source directory for THIS Load() call; the "allowlist" is
// implicitly every discovered skill's qualified name (mirroring this same
// fix's helix_code counterpart, internal/agent/skill_external_policy.go,
// which makes the identical simplification for the same reason: an
// external tier has no separate allowlist file of its own, the directory
// listing itself is the declared set of candidates for policy admission).
//
// HXC-159 F-17 (2026-09-23) redesign: the description-similarity check is
// no longer whole-source fail-closed. A confusable PAIR excludes only its
// alphabetically-later member (candidates are walked in the SAME
// deterministic ascending-Qualified()-identity order the ceiling check
// already sorted them into) — every other, non-conflicting skill in the
// source still admits, even when an unrelated pair elsewhere in the same
// source conflicts. exclusions names every excluded skill + which
// already-admitted skill it conflicted with + the score, mirroring
// pkg/skills.admitBySimilarity byte-for-byte in algorithm (see that
// function's doc comment for the maximal-independent-set proof this port
// preserves: no two mutually confusable skills can ever both appear in
// active).
//
// Ceiling and the C5 sandbox precondition remain WHOLE-SET refusals
// (unchanged by F-17): a ceiling breach is a resource-budget concern over
// the whole candidate set, and the sandbox precondition is a readiness
// gate on the whole admitted set — neither is a pairwise conflict, so
// neither is a candidate for partial admission. On EITHER of those
// refusals active is nil and refusal names exactly which rule fired and
// why; on a genuine similarity conflict, refusal stays nil and active +
// exclusions together describe the partial outcome.
func externalPolicyGate(sourceName string, discovered []*Skill, trust ExternalSourceTrust, sandboxReady bool) (active []*Skill, exclusions []nativeSimilarityExclusion, refusal error) {
	if len(discovered) == 0 {
		return nil, nil, nil
	}

	ordered := make([]*Skill, len(discovered))
	copy(ordered, discovered)
	sort.Slice(ordered, func(i, j int) bool {
		return qualifiedName(sourceName, ordered[i]) < qualifiedName(sourceName, ordered[j])
	})

	if len(ordered) > nativeDefaultCeiling {
		offender := qualifiedName(sourceName, ordered[nativeDefaultCeiling])
		return nil, nil, &nativeCeilingExceededError{
			Ceiling:  nativeDefaultCeiling,
			Count:    len(ordered),
			Offender: offender,
		}
	}

	admitted, excl := nativeAdmitBySimilarity(sourceName, ordered)

	if trust == TrustThirdParty && !sandboxReady {
		offenders := make([]string, len(admitted))
		for i, s := range admitted {
			offenders[i] = qualifiedName(sourceName, s)
		}
		return nil, nil, &nativeThirdPartyBlockedError{Offenders: offenders}
	}

	return admitted, excl, nil
}

// nativeAdmitBySimilarity is the byte-for-byte native port of
// pkg/skills.admitBySimilarity (HXC-159 F-17, 2026-09-23). ordered MUST
// already be sorted in ascending Qualified()-identity order (externalPolicyGate
// guarantees this). Each candidate is checked against the FULL
// admitted-so-far set — not merely its original pairing — so the returned
// admitted slice is a maximal independent set of the pairwise-confusability
// graph: it is impossible for two mutually confusable skills to both
// appear in it. Tie-break rule (mirrors pkg/skills, MUST stay identical):
// for any confusable pair, the alphabetically-first Qualified() identity is
// kept and the later one is excluded.
func nativeAdmitBySimilarity(sourceName string, ordered []*Skill) (admitted []*Skill, exclusions []nativeSimilarityExclusion) {
	for _, c := range ordered {
		conflictsWith := ""
		var score float64
		for _, a := range admitted {
			if s := nativeJaccard(c.Description, a.Description); s > nativeSimilarityThreshold {
				conflictsWith = qualifiedName(sourceName, a)
				score = s
				break
			}
		}
		if conflictsWith != "" {
			exclusions = append(exclusions, nativeSimilarityExclusion{
				Excluded:      qualifiedName(sourceName, c),
				ConflictsWith: conflictsWith,
				Score:         score,
				Threshold:     nativeSimilarityThreshold,
			})
			continue
		}
		admitted = append(admitted, c)
	}
	return admitted, exclusions
}

// logPolicyRefusal is the single place ExternalSkillSource.Load logs a
// policy refusal — kept as a named function so a future caller wanting a
// structured logger (this package already has one, via Service.SetLogger,
// for its OTHER components) can swap the implementation in one place
// without touching externalPolicyGate's pure decision logic.
func logPolicyRefusal(sourceName string, refusal error) {
	log.Printf("⚠️  skills: external source %q refused by policy: %v", sourceName, refusal)
}

// logPolicyExclusions is the single place ExternalSkillSource.Load logs
// per-skill similarity exclusions under the HXC-159 F-17 partial-admission
// model — one auditable line per excluded skill, naming exactly which
// already-admitted skill it conflicted with and at what score, so an
// operator can see what happened and why without re-deriving it from the
// admitted list's absence.
func logPolicyExclusions(sourceName string, exclusions []nativeSimilarityExclusion) {
	for _, ex := range exclusions {
		log.Printf("⚠️  skills: external source %q: %s", sourceName, ex.String())
	}
}
