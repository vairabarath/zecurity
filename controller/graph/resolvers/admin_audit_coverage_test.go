package resolvers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// mutationsAuditedElsewhere lists the mutations that write no audit row in the
// resolver itself, each with the reason.
var mutationsAuditedElsewhere = map[string]string{
	"InitiateAuth":       "anonymous login start; changes no workspace state",
	"TestIdpConnection":  "read-only probe of the IdP; changes nothing",
	"ApproveDevice":      "audited in setDeviceApproval",
	"UnapproveDevice":    "audited in setDeviceApproval",
	"AcceptScimConflict": "audited in scim.DirectoryService.AcceptLink",
	"RejectScimConflict": "audited in scim.DirectoryService.Reject",
	"ReopenScimConflict": "audited in scim.DirectoryService.Reopen",
}

// Every admin change leaves an audit row (live run 2026-10-29, O-2): each
// mutation resolver calls auditAdmin, auditIdp or the audit package, or is
// listed above with the reason. A new mutation without one fails here.
func TestEveryMutationIsAudited(t *testing.T) {
	files, err := filepath.Glob("*.resolvers.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob resolvers: %v (%d files)", err, len(files))
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	var missing []string
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !isMutationResolver(fn.Recv) {
				continue
			}
			name := fn.Name.Name
			seen[name] = true
			if _, ok := mutationsAuditedElsewhere[name]; ok {
				continue
			}
			if !callsAudit(fn.Body) {
				missing = append(missing, path+": "+name)
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("mutation writes no audit row: %s (call r.auditAdmin, or list it in mutationsAuditedElsewhere with the reason)", m)
	}
	for name := range mutationsAuditedElsewhere {
		if !seen[name] {
			t.Errorf("mutationsAuditedElsewhere lists %s, which is no longer a mutation", name)
		}
	}
}

func isMutationResolver(recv *ast.FieldList) bool {
	if len(recv.List) != 1 {
		return false
	}
	star, ok := recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "mutationResolver"
}

func callsAudit(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "auditAdmin", "auditIdp":
			found = true
		case "Record", "RecordTx":
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "audit" {
				found = true
			}
		}
		return !found
	})
	return found
}
