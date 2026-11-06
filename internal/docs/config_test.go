package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func configRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find repo root from test file")
	return ""
}

func configRead(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// configLookupFunctions names every helper in internal/config whose first
// string argument is an environment variable name read by config.Load.
var configLookupFunctions = map[string]bool{
	"lookupConfigValue":        true,
	"lookupSecret":             true,
	"valueOrFallback":          true,
	"parseInt32EnvWithFile":    true,
	"parseDurationEnvWithFile": true,
	"getenv":                   true,
	"get":                      true,
	"varName":                  true,
	"Getenv":                   true,
}

// configUndocumentedAllowlist lists variables read in internal/config that are
// intentionally excluded from the reference table, with the reason for each.
// Keeping this map empty (or strictly justified) ensures no new setting slips
// past docs/configuration.md.
var configUndocumentedAllowlist = map[string]string{}

// configDocMentionAllowlist lists environment-variable-shaped tokens mentioned
// in prose or examples in docs/configuration.md that are not direct lookup keys
// in internal/config (for example AWS S3 SDK variables, per-network suffix
// examples, or test-only variables), along with the reason each appears.
var configDocMentionAllowlist = map[string]string{
	"AWS_ACCESS_KEY_ID":          "standard AWS SDK credential variable mentioned in ARCHIVE_URL S3 prose",
	"AWS_REGION":                 "standard AWS SDK region variable mentioned in ARCHIVE_URL S3 prose",
	"AWS_SECRET_ACCESS_KEY":      "standard AWS SDK credential variable mentioned in ARCHIVE_URL S3 prose",
	"NETWORK_PASSPHRASE_MAINNET": "example of per-network <KEY>_<NETWORK> suffix expansion for NETWORKS",
	"RPC_URL_MAINNET":            "example of per-network <KEY>_<NETWORK> suffix expansion for NETWORKS",
	"RPC_URLS_MAINNET":           "example of per-network <KEY>_<NETWORK> suffix expansion for NETWORKS",
	"TEST_DATABASE_URL":          "test-only variable explicitly listed under Not environment variables",
	"ACME_TOKEN":                 "shell snippet variable used in curl example for WORKSPACE_TOKENS",
	"NEW_TOKEN":                  "shell snippet variable used in rotation example for API_TOKEN",
	"OLD_TOKEN":                  "shell snippet variable used in rotation example for API_TOKEN",
}

// configRequiredPhrases checks structural sections and security notes required
// in docs/configuration.md beyond individual variable names.
var configRequiredPhrases = []string{
	"## Database",
	"## RPC / event source",
	"## HTTP",
	"## Polling",
	"## Logging",
	"**required when `SOURCE_MODE=sorotrail`**",
	"**yes**",
	"single sign-on off",
	"Authorization code with PKCE, not implicit",
	"| `OIDC_CLIENT_SECRET` | **yes** |",
}

var envVarNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*_[A-Z0-9_]+$`)

// deriveConfigEnvVarsFromAST parses every non-test Go source file in dir with
// go/ast and returns the sorted set of environment variable names read by
// configuration lookup calls (plus the slice variables looped over in parseOIDC).
//
// We use go/ast rather than a runtime hook so production code in internal/config
// stays untouched and every branch (including error paths, loadSecrets,
// parseOIDC, ParseNetworks and loadMTLS) is inspected regardless of which
// environment variables happen to be set during the test run.
func deriveConfigEnvVarsFromAST(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config dir %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	found := make(map[string]bool)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		fileAST, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		collectASTEnvVars(fileAST, found)
	}
	return found
}

func collectASTEnvVars(fileAST *ast.File, out map[string]bool) {
	ast.Inspect(fileAST, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			fnName := calledFuncName(node.Fun)
			if !configLookupFunctions[fnName] || len(node.Args) == 0 {
				return true
			}
			if lit, ok := node.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if val, err := strconv.Unquote(lit.Value); err == nil && envVarNameRE.MatchString(val) {
					out[val] = true
				}
			}
		case *ast.RangeStmt:
			// Catch table-driven loops over literal variable names such as the
			// OIDC_* guard in parseOIDC.
			comp, ok := node.X.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, elt := range comp.Elts {
				if lit, ok := elt.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if val, err := strconv.Unquote(lit.Value); err == nil && envVarNameRE.MatchString(val) {
						out[val] = true
					}
				}
			}
		}
		return true
	})
}

func calledFuncName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	default:
		return ""
	}
}

var (
	docTableRowVarRE = regexp.MustCompile(`(?m)^\|\s*` + "`" + `([A-Z][A-Z0-9_]+)` + "`" + `\s*\|`)
	docInlineVarRE   = regexp.MustCompile("`([A-Z][A-Z0-9]*_[A-Z0-9_]+)`")
)

// extractDocVariables returns:
//   - tableVars: every variable documented as the leading column of a markdown table row
//   - allVars: every ENV_VAR-shaped token inside backticks anywhere in docs/configuration.md
func extractDocVariables(doc string) (tableVars map[string]bool, allVars map[string]bool) {
	tableVars = make(map[string]bool)
	for _, m := range docTableRowVarRE.FindAllStringSubmatch(doc, -1) {
		if envVarNameRE.MatchString(m[1]) || m[1] == "NETWORK" || m[1] == "NETWORKS" {
			tableVars[m[1]] = true
		}
	}
	allVars = make(map[string]bool)
	for _, m := range docInlineVarRE.FindAllStringSubmatch(doc, -1) {
		allVars[m[1]] = true
	}
	for k := range tableVars {
		allVars[k] = true
	}
	return tableVars, allVars
}

// Also capture single-word uppercase config variables (like NETWORK and NETWORKS)
// when passed to lookup helpers.
func collectSingleWordASTEnvVars(fileAST *ast.File, out map[string]bool) {
	ast.Inspect(fileAST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fnName := calledFuncName(call.Fun)
		if !configLookupFunctions[fnName] || len(call.Args) == 0 {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if val, err := strconv.Unquote(lit.Value); err == nil && (val == "NETWORK" || val == "NETWORKS" || envVarNameRE.MatchString(val)) {
				out[val] = true
			}
		}
		return true
	})
}

func deriveAllConfigEnvVars(t *testing.T, dir string) map[string]bool {
	t.Helper()
	found := deriveConfigEnvVarsFromAST(t, dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fileAST, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		collectSingleWordASTEnvVars(fileAST, found)
	}
	return found
}

// checkConfigurationDrift compares the AST-derived config variables against
// docs/configuration.md and returns a list of human-readable drift errors.
func checkConfigurationDrift(codeVars map[string]bool, doc string) []string {
	var problems []string
	tableVars, allDocVars := extractDocVariables(doc)

	// 1. Every variable read by internal/config must appear as a documented
	// table entry in docs/configuration.md (unless explicitly allowlisted).
	var codeSorted []string
	for v := range codeVars {
		codeSorted = append(codeSorted, v)
	}
	sort.Strings(codeSorted)

	for _, v := range codeSorted {
		if _, allowed := configUndocumentedAllowlist[v]; allowed {
			if tableVars[v] {
				problems = append(problems, v+" is in configUndocumentedAllowlist but is now documented in docs/configuration.md; remove it from the allowlist")
			}
			continue
		}
		if !tableVars[v] {
			problems = append(problems, "config.Load reads "+v+" but it is missing from docs/configuration.md tables")
		}
	}

	// 2. Every variable documented in docs/configuration.md must still be read
	// by internal/config (unless it is an allowlisted prose/example token).
	var docSorted []string
	for v := range allDocVars {
		docSorted = append(docSorted, v)
	}
	sort.Strings(docSorted)

	for _, v := range docSorted {
		if codeVars[v] {
			if _, allowed := configDocMentionAllowlist[v]; allowed {
				problems = append(problems, v+" is read by internal/config; remove it from configDocMentionAllowlist")
			}
			continue
		}
		if reason, allowed := configDocMentionAllowlist[v]; allowed && strings.TrimSpace(reason) != "" {
			continue
		}
		problems = append(problems, "docs/configuration.md mentions "+v+" which internal/config no longer reads")
	}

	// 3. Required structural sections and security callouts.
	for _, want := range configRequiredPhrases {
		if !strings.Contains(doc, want) {
			problems = append(problems, "docs/configuration.md missing required section or note "+strconv.Quote(want))
		}
	}

	return problems
}

func TestConfigurationReferenceCoversEveryEnvVar(t *testing.T) {
	root := configRepoRoot(t)
	configDir := filepath.Join(root, "internal", "config")
	codeVars := deriveAllConfigEnvVars(t, configDir)
	if len(codeVars) == 0 {
		t.Fatal("derived 0 environment variables from internal/config")
	}

	doc := configRead(t, root, "docs/configuration.md")
	for _, problem := range checkConfigurationDrift(codeVars, doc) {
		t.Error(problem)
	}
}

// TestConfigurationGuardCatchesUndocumentedLookupAndStaleDoc proves that the
// AST derivation and drift check fail in both directions: when a new lookup is
// added to code without documentation, and when a documented variable is no
// longer read by internal/config.
func TestConfigurationGuardCatchesUndocumentedLookupAndStaleDoc(t *testing.T) {
	root := configRepoRoot(t)
	configDir := filepath.Join(root, "internal", "config")
	codeVars := deriveAllConfigEnvVars(t, configDir)
	doc := configRead(t, root, "docs/configuration.md")

	// Direction 1: parse a synthetic Go snippet with a new lookupConfigValue
	// call and verify checkConfigurationDrift flags the undocumented variable.
	fset := token.NewFileSet()
	synthAST, err := parser.ParseFile(fset, "synthetic.go", `
package config
func extra(fileValues map[string]string) {
	_ = lookupConfigValue("SYNTHETIC_UNDOCUMENTED_VAR", fileValues)
}
`, 0)
	if err != nil {
		t.Fatalf("parse synthetic AST: %v", err)
	}
	withExtraCodeVar := make(map[string]bool, len(codeVars)+1)
	for k, v := range codeVars {
		withExtraCodeVar[k] = v
	}
	collectASTEnvVars(synthAST, withExtraCodeVar)
	if !withExtraCodeVar["SYNTHETIC_UNDOCUMENTED_VAR"] {
		t.Fatal("AST extractor did not record SYNTHETIC_UNDOCUMENTED_VAR")
	}
	problems := checkConfigurationDrift(withExtraCodeVar, doc)
	if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "SYNTHETIC_UNDOCUMENTED_VAR") {
		t.Fatalf("expected drift check to fail on undocumented variable SYNTHETIC_UNDOCUMENTED_VAR, got: %v", problems)
	}

	// Direction 2: add a stale variable to the markdown document and verify
	// checkConfigurationDrift flags it as no longer read by internal/config.
	staleDoc := doc + "\n| `STALE_REMOVED_SETTING` | string | empty | optional | Removed setting. |\n"
	problems = checkConfigurationDrift(codeVars, staleDoc)
	if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "STALE_REMOVED_SETTING") {
		t.Fatalf("expected drift check to fail on stale documented variable STALE_REMOVED_SETTING, got: %v", problems)
	}
}

func TestREADMELinksConfigurationReference(t *testing.T) {
	root := configRepoRoot(t)
	readme := configRead(t, root, "README.md")
	if !strings.Contains(readme, "docs/configuration.md") {
		t.Fatal("README.md does not link docs/configuration.md")
	}
}
