package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// surfaceFiles are the read-command surfaces that must reach state only through
// the core API. They are distinct from the flow files, which legitimately run
// git and gh as part of a live run.
var surfaceFiles = []string{"main.go", "stats.go", "watch.go"}

// stateHelpers are the package functions the surfaces used to read state
// directly before #176 routed them through the core API.
var stateHelpers = map[string]bool{
	"loadLedger":     true,
	"fetchNotes":     true,
	"readVerdict":    true,
	"readChecks":     true,
	"discoverAgents": true,
	"loadAgent":      true,
	"gitOut":         true,
	"gitOutRaw":      true,
	"probeDaemon":    true,
	"worktreePaths":  true,
	"eligibleItems":  true,
}

// TestSurfacesReachStateOnlyThroughCore guards the #176 seam: the five read
// commands must obtain every piece of state from the core API and never fall
// back to calling a state helper directly. It walks the surface files' call
// expressions and fails on any direct helper call, so a future edit that wires
// a surface straight to the ledger, notes, git, or tracker is caught here.
func TestSurfacesReachStateOnlyThroughCore(t *testing.T) {
	for _, name := range surfaceFiles {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, name, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var hits []string
		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if stateHelpers[id.Name] {
				hits = append(hits, id.Name)
			}
			return true
		})
		if len(hits) > 0 {
			t.Errorf("%s calls state helper(s) directly (%s); route through the core API instead", name, strings.Join(hits, ", "))
		}
	}
}

// The build/define-time guards for names the surfaces used but that are not
// ordinary function calls in the AST (a Flow selector). They must not appear in
// a surface file at all.
func TestSurfacesDoNotMentionFlowSelector(t *testing.T) {
	for _, name := range surfaceFiles {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), "builderFlow") {
			t.Errorf("%s references the builder Flow selector; route list through the core API instead", name)
		}
	}
}
