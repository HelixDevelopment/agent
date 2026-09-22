// Command skillconformance runs HelixAgent's OWN skillconformance report
// generator (HXC-159 T-P7.01/T-P7.02, RS-12) against real on-disk skill
// directories using the production skills.Service, and writes the
// resulting capability manifest as JSON.
//
// Usage:
//
//	skillconformance -local <dir> [-external <dir>] -out report.json
//
// A blank -external falls back to HELIXSKILLS_EXTERNAL_SOURCE_DIR
// (router.go's own env var, T-P6.01) when set.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"dev.helix.agent/internal/skillconformance"
	"dev.helix.agent/internal/skills"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("skillconformance", flag.ContinueOnError)
	localDir := fs.String("local", "", "local skills directory (required)")
	externalDir := fs.String("external", "", "external skills directory (default: $HELIXSKILLS_EXTERNAL_SOURCE_DIR)")
	outPath := fs.String("out", "", "output path for the JSON report (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *localDir == "" {
		fmt.Fprintln(os.Stderr, "skillconformance: -local is required")
		return 2
	}
	resolvedExternal := *externalDir
	if resolvedExternal == "" {
		resolvedExternal = os.Getenv("HELIXSKILLS_EXTERNAL_SOURCE_DIR")
	}

	cfg := skills.DefaultSkillConfig()
	cfg.SkillsDirectory = *localDir
	cfg.EnableSemanticMatching = false
	cfg.HotReload = false
	svc := skills.NewService(cfg)
	ctx := context.Background()
	if err := svc.Initialize(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "skillconformance: initialize: %v\n", err)
		return 1
	}
	if resolvedExternal != "" {
		if err := svc.LoadSkillsFromPath(ctx, resolvedExternal); err != nil {
			fmt.Fprintf(os.Stderr, "skillconformance: load external: %v\n", err)
			return 1
		}
	}

	authz := skills.NewSkillAuthorizer(svc)
	report := skillconformance.GenerateReport(svc, authz, skillconformance.DirTierMap{
		Local:    *localDir,
		External: resolvedExternal,
	})
	if errs := report.Validate(); len(errs) != 0 {
		fmt.Fprintln(os.Stderr, "skillconformance: generated report failed its own schema validation:")
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %v\n", e)
		}
		return 1
	}

	raw, err := report.MarshalIndentJSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skillconformance: encode: %v\n", err)
		return 1
	}
	if *outPath == "" {
		os.Stdout.Write(raw)
		return 0
	}
	if err := os.WriteFile(*outPath, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "skillconformance: write %q: %v\n", *outPath, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "skillconformance: wrote %s (%d skills)\n", *outPath, len(report.Skills))
	return 0
}
