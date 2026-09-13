package mcp

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ============================================================================
// ExtendedMCPPackages Tests
// ============================================================================

func TestExtendedMCPPackages(t *testing.T) {
	t.Parallel()
	t.Run("ExtendedMCPPackages is not empty", func(t *testing.T) {
		assert.NotEmpty(t, ExtendedMCPPackages)
	})

	// Reconciled 2026-09-03. This used to assert `len >= 30`. The catalogue
	// dropped to 23 when eleven entries naming npm packages that do not exist
	// (mcp-server-replicate, mcp-server-stable-diffusion, mcp-miro,
	// framelink-figma-mcp, lsp-tools-mcp, ...) were removed.
	//
	// A raw size floor is the wrong invariant: the only way to satisfy it is to
	// add entries, and it does not care whether those entries are real. It
	// would have been satisfied by the very fictional packages that made this
	// catalogue unusable. What the catalogue actually owes its callers is that
	// each entry is distinct and names a well-formed npm package — asserted
	// here, with registry existence covered by
	// tests/integration.TestGoSourceMCPPackageExistence.
	t.Run("Entries are distinct and name well-formed npm packages", func(t *testing.T) {
		t.Parallel()
		seen := make(map[string]bool, len(ExtendedMCPPackages))
		for _, pkg := range ExtendedMCPPackages {
			assert.False(t, seen[pkg.Name], "Duplicate package name %q", pkg.Name)
			seen[pkg.Name] = true

			assert.NotContains(t, pkg.NPM, " ", "NPM name %q must not contain spaces", pkg.NPM)
			assert.False(t, strings.HasPrefix(pkg.NPM, "-"),
				"NPM name %q must not start with a flag dash", pkg.NPM)
			if strings.HasPrefix(pkg.NPM, "@") {
				assert.Len(t, strings.Split(pkg.NPM, "/"), 2,
					"Scoped NPM name %q must be exactly @scope/name", pkg.NPM)
			} else {
				assert.NotContains(t, pkg.NPM, "/",
					"Unscoped NPM name %q must not contain a slash", pkg.NPM)
			}
		}
	})

	t.Run("All packages have required fields", func(t *testing.T) {
		t.Parallel()
		for _, pkg := range ExtendedMCPPackages {
			assert.NotEmpty(t, pkg.Name, "Package name should not be empty")
			assert.NotEmpty(t, pkg.NPM, "NPM field should not be empty")
			assert.NotEmpty(t, pkg.Description, "Description should not be empty")
			assert.NotEmpty(t, pkg.Category, "Category should not be empty")
		}
	})

	t.Run("Package categories are valid", func(t *testing.T) {
		t.Parallel()
		validCategories := map[MCPPackageCategory]bool{
			CategoryCore:     true,
			CategoryVectorDB: true,
			CategoryDesign:   true,
			CategoryImage:    true,
			CategoryDev:      true,
			CategorySearch:   true,
			CategoryCloud:    true,
		}

		for _, pkg := range ExtendedMCPPackages {
			assert.True(t, validCategories[pkg.Category],
				"Package %s has invalid category: %s", pkg.Name, pkg.Category)
		}
	})
}

// ============================================================================
// GetPackagesByCategory Tests
// ============================================================================

func TestGetPackagesByCategory(t *testing.T) {
	t.Parallel()
	t.Run("Returns core packages", func(t *testing.T) {
		packages := GetPackagesByCategory(CategoryCore)
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategoryCore, pkg.Category)
		}
	})

	t.Run("Returns vectordb packages", func(t *testing.T) {
		t.Parallel()
		packages := GetPackagesByCategory(CategoryVectorDB)
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategoryVectorDB, pkg.Category)
		}

		// Check for specific packages
		names := make(map[string]bool)
		for _, pkg := range packages {
			names[pkg.Name] = true
		}
		assert.True(t, names["chroma"], "Should include chroma")
		assert.True(t, names["qdrant"], "Should include qdrant")
	})

	t.Run("Returns design packages", func(t *testing.T) {
		t.Parallel()
		packages := GetPackagesByCategory(CategoryDesign)
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategoryDesign, pkg.Category)
		}

		// Check for specific packages
		names := make(map[string]bool)
		for _, pkg := range packages {
			names[pkg.Name] = true
		}
		// Re-reconciled 2026-09-03: "miro" is back. Its old package `mcp-miro`
		// is a proven npm 404, but @k-jarzyna/mcp-miro is real and its declared
		// repository resolves, so the entry was restored under that name.
		assert.True(t, names["figma"], "Should include figma")
		assert.True(t, names["miro"], "Should include miro")
	})

	t.Run("Returns image packages", func(t *testing.T) {
		t.Parallel()
		// Re-reconciled 2026-09-03. This asserted Empty() while every image
		// package was a proven npm 404. "replicate" has since been restored on
		// `replicate-mcp` — published by Replicate themselves — so Empty() now
		// asserts the OLD state and would only pass if the restore were undone.
		//
		// Per the fix-breaks-its-own-gate rule it is reconciled to the real
		// contract rather than deleted or weakened to a tautology: the category
		// is non-empty AND contains only its own members. Break the filter in
		// GetPackagesByCategory and this still fails.
		packages := GetPackagesByCategory(CategoryImage)
		assert.NotEmpty(t, packages)
		for _, pkg := range packages {
			assert.Equal(t, CategoryImage, pkg.Category)
		}
	})

	t.Run("Returns dev packages", func(t *testing.T) {
		t.Parallel()
		packages := GetPackagesByCategory(CategoryDev)
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategoryDev, pkg.Category)
		}

		// Check for specific packages
		names := make(map[string]bool)
		for _, pkg := range packages {
			names[pkg.Name] = true
		}
		assert.True(t, names["postgres"], "Should include postgres")
		assert.True(t, names["redis"], "Should include redis")
	})

	t.Run("Returns search packages", func(t *testing.T) {
		t.Parallel()
		packages := GetPackagesByCategory(CategorySearch)
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategorySearch, pkg.Category)
		}
	})

	t.Run("Returns cloud packages", func(t *testing.T) {
		t.Parallel()
		packages := GetPackagesByCategory(CategoryCloud)
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategoryCloud, pkg.Category)
		}
	})

	t.Run("Returns empty for invalid category", func(t *testing.T) {
		t.Parallel()
		packages := GetPackagesByCategory(MCPPackageCategory("invalid"))
		assert.Empty(t, packages)
	})
}

// ============================================================================
// Convenience Functions Tests
// ============================================================================

func TestGetCorePackages(t *testing.T) {
	t.Parallel()
	t.Run("Returns only core packages", func(t *testing.T) {
		packages := GetCorePackages()
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategoryCore, pkg.Category)
		}
	})

	t.Run("Contains essential packages", func(t *testing.T) {
		t.Parallel()
		packages := GetCorePackages()
		names := make(map[string]bool)
		for _, pkg := range packages {
			names[pkg.Name] = true
		}

		essentialPackages := []string{"filesystem", "github", "memory", "fetch", "puppeteer", "sqlite", "git"}
		for _, name := range essentialPackages {
			assert.True(t, names[name], "Core packages should include %s", name)
		}
	})
}

func TestGetVectorDBPackages(t *testing.T) {
	t.Parallel()
	t.Run("Returns only vectordb packages", func(t *testing.T) {
		packages := GetVectorDBPackages()
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategoryVectorDB, pkg.Category)
		}
	})

	t.Run("Contains expected vector databases", func(t *testing.T) {
		t.Parallel()
		packages := GetVectorDBPackages()
		names := make(map[string]bool)
		for _, pkg := range packages {
			names[pkg.Name] = true
		}

		assert.True(t, names["chroma"], "Should include chroma")
		assert.True(t, names["qdrant"], "Should include qdrant")
		assert.True(t, names["weaviate"], "Should include weaviate")
	})
}

func TestGetDesignPackages(t *testing.T) {
	t.Parallel()
	t.Run("Returns only design packages", func(t *testing.T) {
		packages := GetDesignPackages()
		assert.NotEmpty(t, packages)

		for _, pkg := range packages {
			assert.Equal(t, CategoryDesign, pkg.Category)
		}
	})

	t.Run("Contains expected design tools", func(t *testing.T) {
		t.Parallel()
		packages := GetDesignPackages()
		names := make(map[string]bool)
		for _, pkg := range packages {
			names[pkg.Name] = true
		}

		// Reconciled 2026-09-03: the "miro" entry named `mcp-miro`, which
		// returns HTTP 404 from registry.npmjs.org, and was removed. figma
		// remains because it names `figma-developer-mcp`, which resolves.
		assert.True(t, names["figma"], "Should include figma")
	})
}

// TestGetImagePackages was reconciled 2026-09-03 and re-reconciled the same
// day after the restore.
//
// It originally asserted GetImagePackages() contained "replicate" and
// "stable-diffusion". Every package the image category listed —
// mcp-server-replicate, mcp-server-stable-diffusion, mcp-server-flux,
// mcp-imagesorcery, mcp-server-svgmaker — returned HTTP 404 from
// registry.npmjs.org, so those name-based assertions were replaced with the
// function's real contract: it returns exactly the catalogue entries in its
// category, which is falsifiable (break the filter and this fails) whether the
// category is empty or not.
//
// "replicate" is now restored on `replicate-mcp` (published by Replicate
// themselves; see the catalogue entry for the verification), so the category
// is populated again. The contract assertion below is deliberately left as-is:
// it held while the category was empty and still holds now, which is the point
// of asserting a contract rather than a membership list.
func TestGetImagePackages(t *testing.T) {
	t.Parallel()
	t.Run("Returns exactly the image-category entries", func(t *testing.T) {
		var want []MCPPackage
		for _, pkg := range ExtendedMCPPackages {
			if pkg.Category == CategoryImage {
				want = append(want, pkg)
			}
		}
		assert.Equal(t, want, GetImagePackages())
	})

	t.Run("Returns only image packages", func(t *testing.T) {
		t.Parallel()
		for _, pkg := range GetImagePackages() {
			assert.Equal(t, CategoryImage, pkg.Category)
		}
	})
}

func TestGetAllExtendedPackages(t *testing.T) {
	t.Parallel()
	t.Run("Returns all packages", func(t *testing.T) {
		packages := GetAllExtendedPackages()
		assert.Equal(t, ExtendedMCPPackages, packages)
	})

	t.Run("Contains packages from all categories", func(t *testing.T) {
		t.Parallel()
		packages := GetAllExtendedPackages()
		categories := make(map[MCPPackageCategory]bool)

		for _, pkg := range packages {
			categories[pkg.Category] = true
		}

		// Re-reconciled 2026-09-03. The previous revision asserted
		// CategoryImage was ABSENT, with the explicit exit condition "until a
		// package that actually resolves is found". That condition is now met:
		// `replicate-mcp` resolves, is published by Replicate themselves, and
		// its invocation is documented on replicate.com — so the category is
		// populated again and the old assertion would now only pass if the
		// restore were undone.
		assert.True(t, categories[CategoryCore])
		assert.True(t, categories[CategoryVectorDB])
		assert.True(t, categories[CategoryDesign])
		assert.True(t, categories[CategoryDev])
		assert.True(t, categories[CategorySearch])
		assert.True(t, categories[CategoryCloud])
		assert.True(t, categories[CategoryImage],
			"CategoryImage is populated again by the restored replicate entry")
	})
}

// ============================================================================
// FilterAvailablePackages Tests
// ============================================================================

func TestFilterAvailablePackages(t *testing.T) {
	// Note: cannot use t.Parallel() because subtests use t.Setenv() and os.Unsetenv()
	t.Run("Returns packages with no env requirements", func(t *testing.T) {
		// Ensure no env vars are set for this test
		_ = os.Unsetenv("CHROMA_URL")
		_ = os.Unsetenv("QDRANT_URL")
		_ = os.Unsetenv("FIGMA_ACCESS_TOKEN")

		packages := FilterAvailablePackages(ExtendedMCPPackages)

		// A returned package MAY still declare required env vars: the filter
		// includes it precisely BECAUSE they are set (see
		// FilterAvailablePackages: it returns a package when RequiresEnv is
		// empty OR every var is set). The previous assertion demanded
		// RequiresEnv be EMPTY, which is a different and wrong property — it
		// failed as soon as any package with a satisfied key was available,
		// which happens whenever the developer's shell exports one (measured:
		// TAVILY_API_KEY is set on this host, so tavily is legitimately
		// available and still declares it). Assert the intent the message
		// already stated: every declared var is actually present.
		for _, pkg := range packages {
			for _, envVar := range pkg.RequiresEnv {
				assert.NotEmpty(t, os.Getenv(envVar),
					"Package %s was returned as available but its required var %s is not set",
					pkg.Name, envVar)
			}
		}
	})

	t.Run("Returns packages when env vars are set", func(t *testing.T) {
		// Note: cannot use t.Parallel() with t.Setenv()
		// Set an env var for a package that requires it
		t.Setenv("CHROMA_URL", "http://localhost:8000")

		packages := FilterAvailablePackages(ExtendedMCPPackages)

		// Find chroma package
		var chromaFound bool
		for _, pkg := range packages {
			if pkg.Name == "chroma" {
				chromaFound = true
				break
			}
		}
		assert.True(t, chromaFound, "Chroma should be available when CHROMA_URL is set")
	})

	t.Run("Excludes packages with missing env vars", func(t *testing.T) {
		// Note: cannot use t.Parallel() with os.Unsetenv (modifies global env state)
		// Make sure the env var is NOT set
		_ = os.Unsetenv("FIGMA_ACCESS_TOKEN")

		packages := FilterAvailablePackages(ExtendedMCPPackages)

		// Check that figma is NOT in the list (unless we already set it elsewhere)
		var figmaFound bool
		for _, pkg := range packages {
			if pkg.Name == "figma" {
				figmaFound = true
				break
			}
		}
		assert.False(t, figmaFound, "Figma should not be available when FIGMA_ACCESS_TOKEN is not set")
	})

	t.Run("Handles packages with multiple required env vars", func(t *testing.T) {
		// Note: cannot use t.Parallel() with t.Setenv()
		// S3 requires both AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY
		_ = os.Unsetenv("AWS_ACCESS_KEY_ID")
		_ = os.Unsetenv("AWS_SECRET_ACCESS_KEY")

		packages := FilterAvailablePackages(ExtendedMCPPackages)

		var s3Found bool
		for _, pkg := range packages {
			if pkg.Name == "s3" {
				s3Found = true
				break
			}
		}
		assert.False(t, s3Found, "S3 should not be available when both AWS env vars are not set")

		// Set only one
		t.Setenv("AWS_ACCESS_KEY_ID", "test")
		packages = FilterAvailablePackages(ExtendedMCPPackages)

		s3Found = false
		for _, pkg := range packages {
			if pkg.Name == "s3" {
				s3Found = true
				break
			}
		}
		assert.False(t, s3Found, "S3 should not be available when only one AWS env var is set")

		// Set both
		t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
		packages = FilterAvailablePackages(ExtendedMCPPackages)

		s3Found = false
		for _, pkg := range packages {
			if pkg.Name == "s3" {
				s3Found = true
				break
			}
		}
		assert.True(t, s3Found, "S3 should be available when both AWS env vars are set")
	})

	t.Run("Returns empty list for empty input", func(t *testing.T) {
		t.Parallel()
		packages := FilterAvailablePackages([]MCPPackage{})
		assert.Empty(t, packages)
	})

	t.Run("Returns nil for nil input", func(t *testing.T) {
		t.Parallel()
		packages := FilterAvailablePackages(nil)
		assert.Nil(t, packages)
	})

	t.Run("Works with custom packages", func(t *testing.T) {
		// Note: cannot use t.Parallel() with t.Setenv()
		customPackages := []MCPPackage{
			{Name: "no-env", NPM: "pkg1", Description: "No env", Category: CategoryCore},
			{Name: "with-env", NPM: "pkg2", Description: "With env", Category: CategoryCore, RequiresEnv: []string{"CUSTOM_VAR"}},
		}

		_ = os.Unsetenv("CUSTOM_VAR")
		packages := FilterAvailablePackages(customPackages)
		assert.Len(t, packages, 1)
		assert.Equal(t, "no-env", packages[0].Name)

		t.Setenv("CUSTOM_VAR", "value")
		packages = FilterAvailablePackages(customPackages)
		assert.Len(t, packages, 2)
	})
}

// ============================================================================
// MCPPackageCategory Tests
// ============================================================================

func TestMCPPackageCategory(t *testing.T) {
	t.Parallel()
	t.Run("Category constants are defined correctly", func(t *testing.T) {
		assert.Equal(t, MCPPackageCategory("core"), CategoryCore)
		assert.Equal(t, MCPPackageCategory("vectordb"), CategoryVectorDB)
		assert.Equal(t, MCPPackageCategory("design"), CategoryDesign)
		assert.Equal(t, MCPPackageCategory("image"), CategoryImage)
		assert.Equal(t, MCPPackageCategory("dev"), CategoryDev)
		assert.Equal(t, MCPPackageCategory("search"), CategorySearch)
		assert.Equal(t, MCPPackageCategory("cloud"), CategoryCloud)
	})
}

// ============================================================================
// MCPPackageExtended Tests
// ============================================================================

func TestMCPPackageExtended(t *testing.T) {
	t.Parallel()
	t.Run("Struct fields work correctly", func(t *testing.T) {
		extended := MCPPackageExtended{
			MCPPackage: MCPPackage{
				Name:        "test",
				NPM:         "test-pkg",
				Description: "Test package",
			},
			Category:    CategoryCore,
			RequiresEnv: []string{"TEST_VAR"},
			Optional:    true,
		}

		assert.Equal(t, "test", extended.Name)
		assert.Equal(t, "test-pkg", extended.NPM)
		assert.Equal(t, CategoryCore, extended.Category)
		assert.Equal(t, []string{"TEST_VAR"}, extended.RequiresEnv)
		assert.True(t, extended.Optional)
	})
}

// ============================================================================
// Package RequiresEnv Tests
// ============================================================================

func TestPackageRequiresEnv(t *testing.T) {
	t.Parallel()
	t.Run("VectorDB packages require env vars", func(t *testing.T) {
		packages := GetVectorDBPackages()
		for _, pkg := range packages {
			assert.NotEmpty(t, pkg.RequiresEnv,
				"VectorDB package %s should require env vars", pkg.Name)
		}
	})

	t.Run("Design packages require env vars", func(t *testing.T) {
		t.Parallel()
		packages := GetDesignPackages()
		for _, pkg := range packages {
			if pkg.Name != "imagesorcery" { // Local tool
				assert.NotEmpty(t, pkg.RequiresEnv,
					"Design package %s should require env vars", pkg.Name)
			}
		}
	})

	t.Run("Some core packages have no env requirements", func(t *testing.T) {
		t.Parallel()
		packages := GetCorePackages()
		var noEnvCount int
		for _, pkg := range packages {
			if len(pkg.RequiresEnv) == 0 {
				noEnvCount++
			}
		}
		assert.Greater(t, noEnvCount, 0, "Some core packages should work without env vars")
	})
}

// ============================================================================
// Specific Package Tests
// ============================================================================

func TestSpecificPackages(t *testing.T) {
	t.Parallel()
	t.Run("Filesystem package exists", func(t *testing.T) {
		packages := GetCorePackages()
		var found bool
		for _, pkg := range packages {
			if pkg.Name == "filesystem" {
				found = true
				assert.Equal(t, "@modelcontextprotocol/server-filesystem", pkg.NPM)
				assert.Equal(t, CategoryCore, pkg.Category)
				assert.Empty(t, pkg.RequiresEnv)
				break
			}
		}
		assert.True(t, found, "Filesystem package should exist")
	})

	t.Run("GitHub package exists", func(t *testing.T) {
		t.Parallel()
		packages := GetCorePackages()
		var found bool
		for _, pkg := range packages {
			if pkg.Name == "github" {
				found = true
				assert.Equal(t, "@modelcontextprotocol/server-github", pkg.NPM)
				break
			}
		}
		assert.True(t, found, "GitHub package should exist")
	})

	t.Run("Chroma package has correct URL env var", func(t *testing.T) {
		t.Parallel()
		packages := GetVectorDBPackages()
		for _, pkg := range packages {
			if pkg.Name == "chroma" {
				assert.Contains(t, pkg.RequiresEnv, "CHROMA_URL")
				break
			}
		}
	})

	t.Run("Qdrant package has correct URL env var", func(t *testing.T) {
		t.Parallel()
		packages := GetVectorDBPackages()
		for _, pkg := range packages {
			if pkg.Name == "qdrant" {
				assert.Contains(t, pkg.RequiresEnv, "QDRANT_URL")
				break
			}
		}
	})

	// Reconciled 2026-09-03. This asserted FIGMA_ACCESS_TOKEN, which is the
	// variable the OLD entry declared while pointing at `mcp-server-figma` — an
	// npm security holding package that installs nothing, so nothing ever read
	// it. The real package, figma-developer-mcp, reads FIGMA_API_KEY (confirmed
	// in its published tarball). Keeping the old name would assert a variable
	// no server consults, which is how the entry looked correct while being
	// unusable.
	t.Run("Figma package requires its API key", func(t *testing.T) {
		t.Parallel()
		packages := GetDesignPackages()
		found := false
		for _, pkg := range packages {
			if pkg.Name == "figma" {
				found = true
				assert.Contains(t, pkg.RequiresEnv, "FIGMA_API_KEY")
				break
			}
		}
		// The old loop asserted nothing at all if the entry went missing.
		assert.True(t, found, "figma must be present in the design category")
	})

	t.Run("Brave search requires API key", func(t *testing.T) {
		t.Parallel()
		packages := GetPackagesByCategory(CategorySearch)
		for _, pkg := range packages {
			if pkg.Name == "brave-search" {
				assert.Contains(t, pkg.RequiresEnv, "BRAVE_API_KEY")
				break
			}
		}
	})
}

// ============================================================================
// Benchmark Tests
// ============================================================================

func BenchmarkGetPackagesByCategory(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		GetPackagesByCategory(CategoryCore)
	}
}

func BenchmarkGetAllExtendedPackages(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		GetAllExtendedPackages()
	}
}

func BenchmarkFilterAvailablePackages(b *testing.B) {
	packages := GetAllExtendedPackages()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		FilterAvailablePackages(packages)
	}
}
