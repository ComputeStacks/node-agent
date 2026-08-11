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
	return coveredBy(path, marksFailed)
}

// coveredBy is the branch-aware walk both guards in this file share; see
// coveredByFailedStatus for what "covered" means and why only the statements at or before
// the one the path descends through can count.
func coveredBy(path []ast.Node, matches func(ast.Stmt) bool) bool {
	for i := 0; i+1 < len(path); i++ {
		block, ok := path[i].(*ast.BlockStmt)
		if !ok {
			continue
		}
		for _, stmt := range block.List {
			if stmt.Pos() > path[i+1].Pos() {
				break // past the statement the path descends into
			}
			if matches(stmt) {
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

// TestRestoreFailureBranchesReportAReason is the reason half of the status guard above: a
// failed restore must say WHY, not just that it failed.
//
// runner.go synthesizes errors.New("task reported failure") for any task whose
// EventLog.Status is "failed" without a returned error, and that string is then the whole of
// result_json.error — the field the controller shows for a failed restore. Every failure
// branch in Restore returned nil, so borg's own diagnosis ("(LockTimeout) Failed to
// create/acquire the lock", "big.bin: write: [Errno 28] No space left on device") reached the
// task result only as an output line, if at all, and the error said nothing.
//
// Source-level for the same reasons as TestRestoreFailureBranchesReportFailure, and it
// asserts the same two shapes that test does:
//
//  1. every rollbackRestore branch assigns restoreFailure on the way down to the call. That
//     is the set of branches that cannot return where they discover the failure — they have
//     to put the snapshot back and restart the containers first — so the reason has to be
//     carried in a variable to the single tail return, and each of them has to remember to
//     do it. Asserting the tail return is not literal nil instead would be vacuous: it is
//     `return restoreFailure` whether or not any branch ever assigned it, which is exactly
//     how the postRestore branch could be forgotten with the suite green.
//  2. no return that is covered by `EventLog.Status = "failed"` returns the bare identifier
//     nil. That is what pins the ten branches ABOVE the rollback sites, which return their
//     reason directly.
func TestRestoreFailureBranchesReportAReason(t *testing.T) {
	fset, body := parseRestoreBody(t)

	sites := rollbackCallPaths(body)
	if len(sites) == 0 {
		t.Fatalf("Restore has no branch that calls rollbackRestore; see " +
			"TestRestoreFailureBranchesReportFailure")
	}

	for _, site := range sites {
		if !coveredBy(site.path, assignsRestoreFailure) {
			t.Errorf("the rollbackRestore call at %s has no `restoreFailure = …` assignment "+
				"before it on its own path; this branch rolls the volume back and then falls "+
				"through to `return restoreFailure`, so with nothing assigned the controller is "+
				"told the restore failed with runner.go's synthesized \"task reported failure\" "+
				"and never learns the reason. Assign it BEFORE the rollback call, the way "+
				"EventLog.Status is set, so the reason does not depend on how the rollback goes",
				fset.Position(site.pos))
		}
	}

	for _, ret := range returnPaths(body) {
		if !coveredByFailedStatus(ret.path) {
			continue
		}
		stmt, ok := ret.node.(*ast.ReturnStmt)
		if !ok || len(stmt.Results) != 1 {
			continue
		}
		if id, isIdent := stmt.Results[0].(*ast.Ident); isIdent && id.Name == "nil" {
			t.Errorf("the return at %s is on a branch that set EventLog.Status = \"failed\" but "+
				"returns nil; runner.go then reports the restore as failed with no reason at all "+
				"beyond \"task reported failure\". Return the same reason the branch posted",
				fset.Position(stmt.Pos()))
		}
	}
}

// TestRestoreRefusesFilePathsBeforeAnythingMoves guards the placement of the file_paths
// refusal, which is the whole of its value.
//
// A named-path restore does not restore part of a volume, it destroys the rest of it:
// takeRestoreSnapshot (inside preRestore) moves the WHOLE of /mnt/data into /root/.snapshot
// in the backup container, an include-path extract puts back only what was named, and a
// fully-matching one exits 0 — so the restore is reported COMPLETED and the deferred
// repo.StopContainer() reaps the AutoRemove container with everything that was not named
// still inside it. Refusing the request only helps while nothing has been moved, so the
// refusal has to sit ahead of preRestore, and it has to be the only thing in Restore that
// looks at params.FilePaths.
//
// Same shape as TestPreRestoreStopsContainersBeforeSnapshot: it parses rather than searching
// for a substring, because "before preRestore" is a statement about order and nesting and
// neither is expressible over flat text.
func TestRestoreRefusesFilePathsBeforeAnythingMoves(t *testing.T) {
	fset, body := parseRestoreBody(t)

	// Both markers can be nested inside a top-level statement (the refusal's reference is
	// inside an `if` condition), so record the index of the top-level statement whose
	// SUBTREE contains each one — that index is what the ordering assertion is about.
	const notFound = -1
	refusalIdx, preRestoreIdx, lastFilePathsIdx := notFound, notFound, notFound

	for i, stmt := range body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if types.ExprString(node) == "params.FilePaths" {
					if refusalIdx == notFound {
						refusalIdx = i
					}
					lastFilePathsIdx = i
				}
			case *ast.CallExpr:
				if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "preRestore" && preRestoreIdx == notFound {
					preRestoreIdx = i
				}
			}
			return true
		})
	}

	if refusalIdx == notFound {
		t.Fatalf("Restore never mentions params.FilePaths; the refusal is gone, so a restore " +
			"carrying file_paths runs again — and a successful one leaves the volume holding " +
			"only the named paths, with the rest of it reaped along with the backup container")
	}
	if preRestoreIdx == notFound {
		t.Fatalf("Restore no longer calls preRestore; the refusal's placement is defined " +
			"relative to it because preRestore is what moves the volume aside")
	}

	if refusalIdx >= preRestoreIdx {
		t.Errorf("the params.FilePaths check (stmt %d) does not run before preRestore (stmt %d); "+
			"preRestore has already emptied /mnt/data into the backup container by then, so "+
			"refusing there fails the restore with the volume already gone instead of leaving it "+
			"untouched", refusalIdx, preRestoreIdx)
	}

	// The refusal must be a refusal — an `if` that returns — and not a read of the paths.
	refusal, ok := body.List[refusalIdx].(*ast.IfStmt)
	if !ok {
		t.Fatalf("the first statement mentioning params.FilePaths is a %T, not an `if`; the only "+
			"thing Restore may do with file_paths is refuse the request", body.List[refusalIdx])
	}
	returns := false
	ast.Inspect(refusal.Body, func(n ast.Node) bool {
		if _, isReturn := n.(*ast.ReturnStmt); isReturn {
			returns = true
		}
		return true
	})
	if !returns {
		t.Errorf("the params.FilePaths branch at %s does not return; a refusal that falls "+
			"through carries on into the restore it was meant to stop",
			fset.Position(refusal.Pos()))
	}

	if lastFilePathsIdx > refusalIdx {
		t.Errorf("params.FilePaths is still read at stmt %d, after the refusal at stmt %d; "+
			"nothing downstream may honor it — borg.Archive.Restore keeps the capability and its "+
			"tests, but Restore must pass nil, because it is the side that owns the snapshot",
			lastFilePathsIdx, refusalIdx)
	}
}

// assignsRestoreFailure matches `restoreFailure = …` as a statement in its own right.
//
// Plain assignment only, never `:=`: a short variable declaration inside a failure branch
// declares a NEW restoreFailure scoped to that block, leaves the outer one nil, and compiles
// clean apart from an unused-variable error only if nothing else reads it.
func assignsRestoreFailure(stmt ast.Stmt) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || assign.Tok != token.ASSIGN || len(assign.Lhs) != 1 {
		return false
	}
	return types.ExprString(assign.Lhs[0]) == "restoreFailure"
}

type returnSite struct {
	path []ast.Node
	node ast.Node
}

// returnPaths collects every return in Restore with the chain of nodes above it, so a return
// can be tested against the branch it actually sits in.
func returnPaths(body *ast.BlockStmt) []returnSite {
	var sites []returnSite
	var stack []ast.Node

	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		if _, ok := n.(*ast.ReturnStmt); ok {
			sites = append(sites, returnSite{path: append([]ast.Node(nil), stack...), node: n})
		}
		return true
	})
	return sites
}

// parseRestoreBody parses restore.go and returns Restore's body, for the two guards above
// that need to walk it. TestRestoreFailureBranchesReportFailure inlines the same lookup; it
// is left as it is deliberately, since it is the guard the failure branches are keyed to.
func parseRestoreBody(t *testing.T) (*token.FileSet, *ast.BlockStmt) {
	t.Helper()

	const file = "restore.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "Restore" && fn.Body != nil {
			return fset, fn.Body
		}
	}
	t.Fatalf("no Restore function with a body found in %s", file)
	return nil, nil
}

// TestRollbackOutcomeZeroValue pins the two properties of rollbackOutcome that are
// otherwise held in place by nothing but the order of an iota block and a comment.
//
// The zero value has to be rollbackCleanupFailed. rollbackRestore's PostRestore block
// installs a deferred recover(), and a recovered panic leaves the return unset — so the
// zero value is what a panicking rollback reports. That block runs only after the put-back
// has already succeeded, and (because defer is function-scoped, not block-scoped) so does
// the same recover for a panic in rollbackRestoreMysql/Postgres, which are also post
// put-back. rollbackComplete would log "Completed restore rollback" for a rollback that
// panicked partway; rollbackSnapshotLost would tell an operator the customer's data may be
// gone when it is back in the volume.
//
// Reordering the const block — alphabetising it, or inserting a fourth outcome at the top —
// is a one-line edit that silently breaks this, which is why it is asserted rather than
// asked for in a comment.
func TestRollbackOutcomeZeroValue(t *testing.T) {
	var unset rollbackOutcome
	if unset != rollbackCleanupFailed {
		t.Errorf("the zero value of rollbackOutcome is %d, wanted rollbackCleanupFailed (%d): a "+
			"recovered panic in rollbackRestore leaves the return unset, and by then the snapshot "+
			"is already back in the volume", unset, rollbackCleanupFailed)
	}

	// cleanupOutcome's failure must never be the escalating outcome: every step it wraps
	// runs after the put-back, so its failure is tidying that did not finish, not data at
	// risk.
	if got := cleanupOutcome(false); got != rollbackCleanupFailed {
		t.Errorf("cleanupOutcome(false) = %d, wanted rollbackCleanupFailed (%d)", got, rollbackCleanupFailed)
	}
	if got := cleanupOutcome(true); got != rollbackComplete {
		t.Errorf("cleanupOutcome(true) = %d, wanted rollbackComplete (%d)", got, rollbackComplete)
	}
}

// TestOnlyALostSnapshotEscalatesTheReason is the regression guard for the bug this outcome
// type was introduced to fix: rollbackFailure — "the volume's contents were not put back" —
// must be reachable ONLY from a lost snapshot.
//
// rollbackRestore's PostRestore hook cannot succeed on this path at all (ServiceExec
// resolves only running containers and Restore has just stopped them), so for every volume
// that configures one, rollbackCleanupFailed is the outcome of EVERY rollback. Escalate
// that and the loudest message in the restore path becomes the routine one, and the day the
// put-back really fails it has already been trained into the background.
func TestOnlyALostSnapshotEscalatesTheReason(t *testing.T) {
	fset, body := parseRestoreBody(t)

	total := countCalls(body, "rollbackFailure")
	if total == 0 {
		t.Fatal("Restore never calls rollbackFailure; a restore whose snapshot was not put back " +
			"no longer says so in the reason it reports, and that is the one outcome that needs a " +
			"human before repo.StopContainer() reaps the backup container")
	}

	// Every call must sit inside a `case rollbackSnapshotLost:` clause.
	guarded := 0
	ast.Inspect(body, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expr := range clause.List {
			if id, isIdent := expr.(*ast.Ident); !isIdent || id.Name != "rollbackSnapshotLost" {
				continue
			}
			for _, stmt := range clause.Body {
				guarded += countCalls(stmt, "rollbackFailure")
			}
		}
		return true
	})

	if guarded != total {
		t.Errorf("Restore calls rollbackFailure %d time(s) but only %d of those are inside a "+
			"`case rollbackSnapshotLost:` clause (%s); a rollback that put the volume back and "+
			"then failed to tidy up must not report that the customer's data was not put back",
			total, guarded, fset.Position(body.Pos()))
	}
}

// TestRestoreReturnsTheCarriedReason pins Restore's tail return.
//
// It is the other half of TestRestoreFailureBranchesReportAReason, which asserts that each
// branch that has to roll back ASSIGNS restoreFailure — and would still pass if the tail
// return were changed back to nil, because restoreFailure stays referenced by
// rollbackFailure(restoreFailure) and so does not trip the unused-variable rule. That
// one-line edit would send all three rollback branches back to reporting runner.go's
// synthesized "task reported failure", which is the whole of what this change exists to fix.
func TestRestoreReturnsTheCarriedReason(t *testing.T) {
	fset, body := parseRestoreBody(t)

	if len(body.List) == 0 {
		t.Fatal("Restore has an empty body")
	}
	last := body.List[len(body.List)-1]
	ret, ok := last.(*ast.ReturnStmt)
	if !ok {
		t.Fatalf("Restore's last statement is a %T, not a return; the three branches that roll "+
			"back carry their reason in restoreFailure to a single tail return", last)
	}
	if len(ret.Results) != 1 || types.ExprString(ret.Results[0]) != "restoreFailure" {
		t.Errorf("Restore's tail return at %s is not `return restoreFailure`; every branch that "+
			"rolls the volume back reaches it, and returning anything else there discards the "+
			"reason they assigned and reports \"task reported failure\" instead",
			fset.Position(ret.Pos()))
	}
}

// TestPreRestoreFailureRecoversBeforeTheContainerGoes guards the ordering that is the whole
// of the pre-restore failure branch's value.
//
// takeRestoreSnapshot moves /mnt/data into /root/.snapshot, and that is a cross-device
// copy-and-unlink because the snapshot lives on the backup container's own writable layer
// (see restore_hooks.go). So a preRestore that fails there fails with the volume's contents
// split between the two, and the container is created with AutoRemove: repo.StopContainer()
// destroys everything already moved. This branch used to call it on its first line, which
// turned a partial move into permanent loss of exactly the part that had moved.
//
// SCOPED to the `if !preRestoreSuccess` block deliberately. Restore has a deferred
// repo.StopContainer() near the top plus three more direct calls in the failure branches
// above this one, so a guard phrased as "the first StopContainer call in Restore" would pass
// no matter what this branch does — and would then get loosened rather than fixed the day it
// started failing.
func TestPreRestoreFailureRecoversBeforeTheContainerGoes(t *testing.T) {
	fset, body := parseRestoreBody(t)
	branch := preRestoreFailureBranch(t, body)

	recoveries := callPositions(branch.Body, "recoverPartialSnapshot")
	if len(recoveries) == 0 {
		t.Fatal("the `if !preRestoreSuccess` branch does not call recoverPartialSnapshot; a snapshot " +
			"move that failed partway leaves the volume's contents in /root/.snapshot, and the " +
			"AutoRemove backup container is stopped moments later with them still inside it")
	}
	stops := callPositions(branch.Body, "repo.StopContainer")
	if len(stops) == 0 {
		t.Fatal("the `if !preRestoreSuccess` branch no longer calls repo.StopContainer; the branch " +
			"returns before Restore's deferred stop is the only one left, which is fine — but this " +
			"guard's ordering assertion has nothing to hold, so re-express it against whatever " +
			"reaps the container now")
	}

	if recoveries[0] > stops[0] {
		t.Errorf("recoverPartialSnapshot (%s) runs after repo.StopContainer (%s); the container is "+
			"AutoRemove and /root/.snapshot is inside it, so by then whatever the failed snapshot "+
			"move had already copied out of the volume is gone for good",
			fset.Position(recoveries[0]), fset.Position(stops[0]))
	}
}

// TestPreRestoreFailureRestartsOnlyWhenRecovered guards the other half of the branch, which
// no test of the returned error can see: whether the customer's service comes back up.
//
// Before the fix this branch returned early, ahead of Restore's tail restart loop, so a
// failed pre-restore left every container of the service stopped indefinitely — nothing in
// the reported reason says so.
//
// The restart is conditional and that is not an accident either. If the put-back did not
// complete, /mnt/data can be materially emptier than the customer left it, and a
// mysql/mariadb entrypoint reads an empty datadir as a first run: it initialises a fresh
// empty database into the volume and the application may take writes on top of it, while
// the only complete copy is in a container that has just been reaped. So this asserts both
// that startServiceContainers is reached AND that every call to it sits under the recovery's
// own result — an unconditional restart passes the first half and is the more dangerous of
// the two mistakes.
func TestPreRestoreFailureRestartsOnlyWhenRecovered(t *testing.T) {
	fset, body := parseRestoreBody(t)
	branch := preRestoreFailureBranch(t, body)

	// The name the recovery's result is bound to, so the guard is about that value rather
	// than about a variable that happens to be spelled "recovered".
	recovered := ""
	ast.Inspect(branch.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || recovered != "" {
			return true
		}
		if len(callPositions(assign.Rhs[0], "recoverPartialSnapshot")) == 0 {
			return true
		}
		if id, isIdent := assign.Lhs[0].(*ast.Ident); isIdent {
			recovered = id.Name
		}
		return true
	})
	if recovered == "" {
		t.Fatal("the `if !preRestoreSuccess` branch does not bind recoverPartialSnapshot's result to " +
			"a variable; its bool is the only report there is that the volume's contents came back, " +
			"and both the reason this branch returns and whether the service is restarted depend on it")
	}

	starts := callPositions(branch.Body, "startServiceContainers")
	if len(starts) == 0 {
		t.Fatal("the `if !preRestoreSuccess` branch never calls startServiceContainers; it returns " +
			"before Restore's tail restart, so the customer's service stays stopped indefinitely " +
			"after a failed pre-restore — and nothing in the reported reason says so")
	}

	guarded := 0
	ast.Inspect(branch.Body, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if id, isIdent := ifStmt.Cond.(*ast.Ident); !isIdent || id.Name != recovered {
			return true
		}
		guarded += len(callPositions(ifStmt.Body, "startServiceContainers"))
		return true
	})

	if guarded != len(starts) {
		t.Errorf("the `if !preRestoreSuccess` branch calls startServiceContainers %d time(s) but only "+
			"%d of those are under `if %s` (%s); starting the service over a volume the recovery could "+
			"not put back lets a mysql entrypoint initialise a fresh empty datadir into it and take "+
			"writes, on top of the customer's data, while the only complete copy is being reaped",
			len(starts), guarded, recovered, fset.Position(branch.Pos()))
	}
}

// preRestoreFailureBranch returns Restore's `if !preRestoreSuccess { … }` block, which is
// what the two guards above are scoped to. Matching the condition rather than a statement
// index keeps them attached to the branch through edits above it, and fails loudly rather
// than vacuously if the branch is renamed or restructured away.
func preRestoreFailureBranch(t *testing.T, body *ast.BlockStmt) *ast.IfStmt {
	t.Helper()

	var found *ast.IfStmt
	ast.Inspect(body, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || found != nil {
			return true
		}
		if types.ExprString(ifStmt.Cond) == "!preRestoreSuccess" {
			found = ifStmt
		}
		return true
	})
	if found == nil {
		t.Fatalf("Restore has no `if !preRestoreSuccess` branch; preRestore's failure is what leaves " +
			"the volume's contents split between /mnt/data and the AutoRemove backup container, and " +
			"the branch that handles it is where the recovery and the restart live")
	}
	return found
}

// callPositions collects the position of every call to name under n, where name is the
// rendered callee — so it matches a method call like "repo.StopContainer" as well as a plain
// function. countCalls below is the ident-only counter the escalation guard uses.
func callPositions(n ast.Node, name string) []token.Pos {
	var out []token.Pos
	ast.Inspect(n, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && types.ExprString(call.Fun) == name {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// countCalls counts calls to a plain (non-method) function named name anywhere under n.
func countCalls(n ast.Node, name string) int {
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, isIdent := call.Fun.(*ast.Ident); isIdent && id.Name == name {
			count++
		}
		return true
	})
	return count
}
