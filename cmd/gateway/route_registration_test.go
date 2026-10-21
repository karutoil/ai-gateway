package main

// Route-registration guard for the NEW protocol surfaces.
//
// main() wires every gateway endpoint inline inside one chi Group that carries
// GatewayAuth + budget + rate-limit middleware. Nothing in the type system
// stops a future edit from registering /v1/images/* or /v1/videos/* OUTSIDE
// that group (which would silently create an unauthenticated, unmetered escape
// hatch), so this test parses cmd/gateway/main.go and asserts the middleware
// chain and the routes co-exist in the same closure.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// authGroupRoutes parses main.go and returns the method+path pairs registered
// inside the func literal that applies the gateway-auth middleware.
func authGroupRoutes(t *testing.T) (map[string]bool, []string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var routes map[string]bool
	var middlewareCalls []string

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		// Does this closure install the gateway auth middleware?
		authd, budgeted, limited := false, false, false
		ast.Inspect(lit, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "middleware" {
				switch sel.Sel.Name {
				case "GatewayAuthWithJWTRevocation":
					authd = true
				case "GatewayRateLimitWithLimits":
					limited = true
				}
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "budget" && sel.Sel.Name == "Middleware" {
				budgeted = true
			}
			return true
		})
		if !authd {
			return true
		}
		if budgeted {
			middlewareCalls = append(middlewareCalls, "budget.Middleware")
		}
		if limited {
			middlewareCalls = append(middlewareCalls, "middleware.GatewayRateLimitWithLimits")
		}
		routes = map[string]bool{}
		ast.Inspect(lit, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Post", "Get", "Delete", "Put", "Patch":
				if len(call.Args) == 0 {
					return true
				}
				if bl, ok := call.Args[0].(*ast.BasicLit); ok && bl.Kind == token.STRING {
					path, err := strconv.Unquote(bl.Value)
					if err == nil {
						routes[sel.Sel.Name+" "+path] = true
					}
				}
			}
			return true
		})
		return false // the innermost matching closure is the gateway group
	})

	if routes == nil {
		t.Fatal("no func literal installs middleware.GatewayAuthWithJWTRevocation in main.go")
	}
	return routes, middlewareCalls
}

// TestNewEndpointsRegisteredInsideAuthGroup is the security-critical assertion:
// every new endpoint must live in the authenticated, rate-limited group.
func TestNewEndpointsRegisteredInsideAuthGroup(t *testing.T) {
	routes, middlewareCalls := authGroupRoutes(t)

	for _, want := range []string{
		"Post /v1/images/generations", "Post /images/generations",
		"Post /v1/images/edits", "Post /images/edits",
		"Post /v1/videos", "Post /videos",
		"Get /v1/videos", "Get /videos",
		"Get /v1/videos/{id}", "Get /videos/{id}",
		"Get /v1/videos/{id}/content", "Get /videos/{id}/content",
		"Delete /v1/videos/{id}", "Delete /videos/{id}",
		"Post /v1/systemone", "Post /systemone",
	} {
		if !routes[want] {
			t.Errorf("%q is not registered inside the gateway-auth group (unauthenticated escape hatch!)", want)
		}
	}

	// The group must still carry budget + rate limiting, not just auth.
	sort.Strings(middlewareCalls)
	if len(middlewareCalls) < 2 {
		t.Fatalf("gateway group is missing middleware: %v", middlewareCalls)
	}
	joined := ""
	for _, m := range middlewareCalls {
		joined += m + " "
	}
	for _, want := range []string{"budget.Middleware", "middleware.GatewayRateLimitWithLimits"} {
		found := false
		for _, m := range middlewareCalls {
			if m == want {
				found = true
			}
		}
		if !found {
			t.Errorf("gateway group is missing %s (got %s)", want, joined)
		}
	}

	// The existing text endpoints must stay registered next to the new ones, so
	// a refactor cannot move one family out of the protected group unnoticed.
	for _, want := range []string{
		"Post /v1/chat/completions", "Post /v1/completions",
		"Post /v1/embeddings", "Post /v1/messages", "Post /v1/responses",
		"Get /v1/models",
	} {
		if !routes[want] {
			t.Errorf("%q is no longer registered inside the gateway-auth group", want)
		}
	}
}

// TestNewEndpointsHaveBareAliases proves each new endpoint is reachable with and
// without the /v1 prefix, matching the documented SDK-baseURL flexibility.
func TestNewEndpointsHaveBareAliases(t *testing.T) {
	routes, _ := authGroupRoutes(t)
	pairs := [][2]string{
		{"Post /v1/images/generations", "Post /images/generations"},
		{"Post /v1/images/edits", "Post /images/edits"},
		{"Post /v1/videos", "Post /videos"},
		{"Get /v1/videos", "Get /videos"},
		{"Get /v1/videos/{id}", "Get /videos/{id}"},
		{"Get /v1/videos/{id}/content", "Get /videos/{id}/content"},
		{"Delete /v1/videos/{id}", "Delete /videos/{id}"},
		{"Post /v1/systemone", "Post /systemone"},
	}
	for _, p := range pairs {
		if !routes[p[0]] {
			t.Errorf("missing %q", p[0])
		}
		if !routes[p[1]] {
			t.Errorf("missing bare alias %q for %q", p[1], p[0])
		}
	}
}

// TestMCPAPICarriesPolicyChain asserts the unified MCP plane is wrapped in the
// same policy chain /v1 requires: authentication, budget and rate limiting.
// Without this, mounting an MCP route could silently create an unauthenticated,
// unmetered escape hatch.
func TestMCPAPICarriesPolicyChain(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// 1. The MCP route table is a chi router.
	foundRoutes := false
	ast.Inspect(file, func(n ast.Node) bool {
		asg, ok := n.(*ast.AssignStmt)
		if !ok || len(asg.Lhs) != 1 || len(asg.Rhs) != 1 {
			return true
		}
		if id, ok := asg.Lhs[0].(*ast.Ident); !ok || id.Name != "mcpRoutes" {
			return true
		}
		foundRoutes = true
		return true
	})
	if !foundRoutes {
		t.Fatal("mcpRoutes router is not constructed in main.go")
	}

	// 2. mcpPlane wraps mcpRoutes in the policy chain.
	var installed []string
	ast.Inspect(file, func(n ast.Node) bool {
		asg, ok := n.(*ast.AssignStmt)
		if !ok || len(asg.Lhs) != 1 || len(asg.Rhs) != 1 {
			return true
		}
		if id, ok := asg.Lhs[0].(*ast.Ident); !ok || id.Name != "mcpPlane" {
			return true
		}
		var rec func(expr ast.Expr)
		rec = func(expr ast.Expr) {
			switch e := expr.(type) {
			case *ast.CallExpr:
				// Middleware composes as mw(auth)(budget)(handler), so the
				// callee itself may be a call: record the selector and recurse
				// into the callee and every argument.
				if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok {
						installed = append(installed, pkg.Name+"."+sel.Sel.Name)
					}
				}
				rec(e.Fun)
				for _, a := range e.Args {
					rec(a)
				}
			}
		}
		rec(asg.Rhs[0])
		return true
	})
	for _, want := range []string{"mcpEndpoint.Auth", "budget.Middleware", "middleware.GatewayRateLimitWithLimits"} {
		ok := false
		for _, got := range installed {
			if got == want {
				ok = true
			}
		}
		if !ok {
			t.Errorf("mcpPlane is missing %s (composed: %v)", want, installed)
		}
	}
}

// TestMCPRoutesMountedOnRootTree guards the bug class where an MCP route was
// registered only inside a private sub-router and therefore never reachable:
// the request fell through to the SPA catch-all and answered 200 with HTML.
// Every MCP path must appear as a mount on the ROOT router.
func TestMCPRoutesMountedOnRootTree(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	mounted := map[string]bool{}
	// Collect r.Handle("<path>", ...) calls on the root router.
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "r" {
			return true
		}
		if sel.Sel.Name != "Handle" && sel.Sel.Name != "Get" && sel.Sel.Name != "Post" {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok {
			mounted[strings.Trim(lit.Value, `"`)] = true
		}
		return true
	})
	// Paths registered through the loop over a string slice.
	ast.Inspect(file, func(n ast.Node) bool {
		rangeStmt, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		composite, ok := rangeStmt.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range composite.Elts {
			if lit, ok := elt.(*ast.BasicLit); ok {
				mounted[strings.Trim(lit.Value, `"`)] = true
			}
		}
		return true
	})

	for _, want := range []string{
		"/mcp",
		"/{server}/mcp",
		"/mcp/{server}",
		"/v1/mcp/server",
		"/mcp-rest/tools/list",
		"/mcp-rest/tools/call",
	} {
		if !mounted[want] {
			t.Errorf("MCP route %q is not mounted on the root router (mounted: %v)", want, sortedKeys(mounted))
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
