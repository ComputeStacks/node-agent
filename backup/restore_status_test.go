package backup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// TestRestoreFailureBranchesReportFailure guards the one thing a restore must never get
// wrong: a restore that did not restore must not be reported as a success.
//
// The controller learns a task's outcome from its terminal status alone (v3.0.0: no live
// csevent stream), and that status is derived from progress.EventLog.Status by
// progress.Failed() in runner.go — RunTask turns a "failed"/"cancelled" status into an
// error and the worker stores the task as FAILED. A failure branch that rolls the volume
// back but leaves the status at its initial "running" therefore reports COMPLETED, with
// the real diagnostic sitting unread in the result output. For a clone that is worse than
// a wrong log line: VolumeServices::CloneStepService reads the task status, marks the clone
// job completed, and tells the customer their volume was restored when it was rolled back
// to empty.
//
// Like TestPreRestoreStopsContainersBeforeSnapshot, this is a source-level guard because
// Restore cannot be unit-tested — it needs a store, a docker client and a borg repository,
// and backup/borg/exec.go records the house position against a docker fake.
//
// It asserts over EVERY branch that calls rollbackRestore rather than over the postRestore
// branch alone. Rolling back is the agent's own admission that the restore did not stand,
// so the two must not be able to drift apart: any future branch that gains a rollback and
// forgets the status lands in exactly the same silent-success hole this one was in.
func TestRestoreFailureBranchesReportFailure(t *testing.T) {
	const file = "restore.go"

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var body *ast.BlockStmt
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "Restore" && fn.Body != nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("no Restore function with a body found in %s", file)
	}

	sites := rollbackCallPaths(body)
	if len(sites) == 0 {
		t.Fatalf("Restore has no branch that calls rollbackRestore; a failed restore that does " +
			"not put the snapshot back leaves the volume as the failure left it, and the " +
			"snapshot dies with the AutoRemove backup container moments later")
	}

	for _, site := range sites {
		if !coveredByFailedStatus(site.path) {
			t.Errorf("the rollbackRestore call at %s is not covered by "+
				"`projectEvent.EventLog.Status = \"failed\"`; runner.go derives the task's "+
				"terminal status from that field, so this branch reports the restore "+
				"COMPLETED to the controller while the volume has just been rolled back",
				fset.Position(site.pos))
		}
	}
}

type rollbackSite struct {
	// path is the chain of AST nodes from Restore's body down to the call, which is what
	// makes the assertion branch-aware: a status assignment counts only when it is on this
	// path's own blocks, never when it sits in a sibling branch of the same `if`.
	path []ast.Node
	pos  token.Pos
}

func rollbackCallPaths(body *ast.BlockStmt) []rollbackSite {
	var sites []rollbackSite
	var stack []ast.Node

	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "rollbackRestore" {
			sites = append(sites, rollbackSite{path: append([]ast.Node(nil), stack...), pos: call.Pos()})
		}
		return true
	})
	return sites
}

// coveredByFailedStatus reports whether the status is set to "failed" on the way down to the
// call. At every block on the path it considers only that block's OWN statements, and only
// those at or before the one the path continues through: an assignment made in the branch
// (like the postRestore branch's) or ahead of the branch in the same block (like
// failedToStop's, which sits before the `if` that rolls back) counts; one in a sibling
// branch, or after the rollback, does not.
func coveredByFailedStatus(path []ast.Node) bool {
	for i := 0; i+1 < len(path); i++ {
		block, ok := path[i].(*ast.BlockStmt)
		if !ok {
			continue
		}
		for _, stmt := range block.List {
			if stmt.Pos() > path[i+1].Pos() {
				break // past the statement the path descends into
			}
			if marksFailed(stmt) {
				return true
			}
		}
	}
	return false
}

// marksFailed matches `projectEvent.EventLog.Status = "failed"` as a statement in its own
// right. It compares the rendered left-hand side, so an assignment to some other Status
// field does not satisfy the guard.
func marksFailed(stmt ast.Stmt) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	if types.ExprString(assign.Lhs[0]) != "projectEvent.EventLog.Status" {
		return false
	}
	lit, ok := assign.Rhs[0].(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && lit.Value == `"failed"`
}
