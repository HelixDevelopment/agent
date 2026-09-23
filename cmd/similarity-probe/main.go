// Command similarity-probe is a small, permanent diagnostic + cross-
// implementation parity harness for the HXC-159 F-17 (2026-09-23) per-skill
// (partial) admission redesign.
//
// It loads every SKILL.md manifest found under the directory given as its
// single argument via ExternalSkillSource — HelixAgent's NATIVE,
// non-importing reimplementation of pkg/skills.Registry.Activate's policy
// engine (D-4 forbids this module from Go-importing
// github.com/HelixDevelopment/skills; see internal/skills/external_policy.go)
// — and prints a single JSON object to stdout:
//
//	{
//	  "admitted": ["name-a", "name-b", ...],
//	  "excluded": [{"excluded":"name-c","conflicts_with":"name-a","score":0.62,"threshold":0.0709}, ...],
//	  "refusal": "" | "<error text, e.g. a ceiling/sandbox whole-set refusal>"
//	}
//
// This is the deliberate counterpart to pkg/skills' OWN
// cmd/similarity-probe (submodules/skills/cmd/similarity-probe) — feeding
// BOTH binaries the identical on-disk SKILL.md fixture and diffing their
// JSON output proves the two independently-maintained implementations
// reach byte-identical admit/exclude decisions without either one
// importing the other (the parity test this task requires; see
// docs/qa/hxc159_f17_partial_admission_*/).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	skillspkg "dev.helix.agent/internal/skills"
)

type exclusionJSON struct {
	Excluded      string  `json:"excluded"`
	ConflictsWith string  `json:"conflicts_with"`
	Score         float64 `json:"score"`
	Threshold     float64 `json:"threshold"`
}

type probeResult struct {
	Admitted []string        `json:"admitted"`
	Excluded []exclusionJSON `json:"excluded"`
	Refusal  string          `json:"refusal"`
}

func run(dir string) (probeResult, error) {
	src := skillspkg.NewExternalSkillSource("probe", dir, nil)
	tools, exclusions, refusal, err := src.LoadForProbe()
	if err != nil {
		return probeResult{}, err
	}
	out := probeResult{}
	if refusal != nil {
		out.Refusal = refusal.Error()
		return out, nil
	}
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	sort.Strings(names)
	out.Admitted = names
	for _, ex := range exclusions {
		out.Excluded = append(out.Excluded, exclusionJSON{
			Excluded:      bareName(ex.Excluded),
			ConflictsWith: bareName(ex.ConflictsWith),
			Score:         ex.Score,
			Threshold:     ex.Threshold,
		})
	}
	sort.Slice(out.Excluded, func(i, j int) bool { return out.Excluded[i].Excluded < out.Excluded[j].Excluded })
	return out, nil
}

// bareName strips a leading "probe." qualifier (the fixed source name this
// probe always registers under) from a qualified <source>.<name> identity.
func bareName(qualified string) string {
	const prefix = "probe."
	if len(qualified) > len(prefix) && qualified[:len(prefix)] == prefix {
		return qualified[len(prefix):]
	}
	return qualified
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: similarity-probe <skill-directory>")
		os.Exit(2)
	}
	result, err := run(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "similarity-probe: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "similarity-probe: encode: %v\n", err)
		os.Exit(1)
	}
}
