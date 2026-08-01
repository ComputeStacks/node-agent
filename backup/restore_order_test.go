package backup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestPreRestoreStopsContainersBeforeSnapshot guards the order of the three steps in
// preRestore that cannot be reordered without losing customer data:
//
//  1. the user's vol.PreRestore command, against a still-running service
//  2. stopServiceContainers
//  3. takeRestoreSnapshot
//
// Moving the stop later loses data: snapshotPath is in the backup container's own
// filesystem rather than in the volume, so takeRestoreSnapshot is a cross-device copy
// followed by an unlink. Under a live writer it produces a torn snapshot and an emptied
// volume, and a failed restore then rolls back to the tear. That was the state of every
// default-strategy volume before the stop was hoisted here — only mysql, mariadb and
// postgres stopped anything, from inside the strategy switch.
//
// Moving the stop earlier breaks every configured PreRestore hook: containermgr.ServiceExec
// resolves its container with FindByService(…, allowOff: false), which collects only
// containers whose state is "running", so with the service already down the hook fails with
// "no containers found".
//
// Like TestTaskHandlersHaveNoRecover and TestBackupPathUsesBackupContinueOnError, this is a
// source-level guard, because preRestore cannot be unit-tested: neither containermgr nor
// backup/borg exposes an interface, takeRestoreSnapshot calls RunShell on a concrete
// *borg.Repository, ServiceExec is a free function, and backup/borg/exec.go records the
// house position against introducing a docker fake to get around that.
//
// It parses, where those two use strings.Contains, and the reason is specific: a substring
// or strings.Index check would pass for a stop moved back INSIDE the strategy switch, which
// is the single most likely regression precisely because that is where the stop used to
// live. Ordering relative to a sibling statement and non-nesting inside a switch are the
// entire invariant, and neither is expressible over a flat string. So the ordering half of
// this test walks the AST. The cheaper half below stays a substring check, because it is
// asking a question about text and not about structure.
func TestPreRestoreStopsContainersBeforeSnapshot(t *testing.T) {
	const file = "restore_hooks.go"

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var body *ast.BlockStmt
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "preRestore" && fn.Body != nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("no preRestore function with a body found in %s", file)
	}

	// The three calls are not all top-level statements of preRestore: the ServiceExec call
	// is nested inside the `if len(vol.PreRestore) > 2 {` block. So for each call, record
	// the index of the top-level statement whose SUBTREE contains it, which is what the
	// ordering assertions are really about.
	const notFound = -1
	idx := map[string]int{
		"containermgr.ServiceExec": notFound,
		"stopServiceContainers":    notFound,
		"takeRestoreSnapshot":      notFound,
	}
	switchIdx := notFound
	stopStmt := ast.Stmt(nil)

	for i, stmt := range body.List {
		if _, ok := stmt.(*ast.SwitchStmt); ok && switchIdx == notFound {
			switchIdx = i
		}
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				if pkg, ok := fun.X.(*ast.Ident); ok {
					name = pkg.Name + "." + fun.Sel.Name
				}
			}
			if at, tracked := idx[name]; tracked && at == notFound {
				idx[name] = i
				if name == "stopServiceContainers" {
					stopStmt = stmt
				}
			}
			return true
		})
	}

	// 1. All three steps are present.
	if idx["containermgr.ServiceExec"] == notFound {
		t.Fatalf("preRestore no longer calls containermgr.ServiceExec; the user's vol.PreRestore " +
			"command is how a customer quiesces their own application before a restore, and " +
			"dropping it silently skips it")
	}
	if idx["stopServiceContainers"] == notFound {
		t.Fatalf("preRestore does not call stopServiceContainers; takeRestoreSnapshot then " +
			"cross-device-copies /mnt/data out from under a running service, which tears the " +
			"snapshot, empties the volume, and makes the rollback restore the tear")
	}
	if idx["takeRestoreSnapshot"] == notFound {
		t.Fatalf("preRestore does not call takeRestoreSnapshot; without it a failed restore has " +
			"no snapshot to roll back to and the volume is left as the failure left it")
	}
	if switchIdx == notFound {
		t.Fatalf("preRestore has no strategy switch; the per-strategy hooks are unreachable")
	}

	// 2. The user's hook runs while the service is still up.
	if idx["containermgr.ServiceExec"] >= idx["stopServiceContainers"] {
		t.Errorf("stopServiceContainers (stmt %d) runs at or before the vol.PreRestore hook "+
			"(stmt %d); ServiceExec resolves containers with FindByService(…, allowOff: false), "+
			"which sees only running ones, so every configured PreRestore command would fail "+
			"with \"no containers found\"",
			idx["stopServiceContainers"], idx["containermgr.ServiceExec"])
	}

	// 3. The stop is one step every restore runs, not a case inside the strategy switch.
	if idx["stopServiceContainers"] <= switchIdx {
		t.Errorf("stopServiceContainers (stmt %d) is at or inside the strategy switch (stmt %d); "+
			"the stop must run for EVERY strategy — that it only ever ran for mysql, mariadb and "+
			"postgres is the bug, because a default-strategy volume then watched /mnt/data empty "+
			"out beneath it while it was still writing",
			idx["stopServiceContainers"], switchIdx)
	}

	// 4. The stop precedes the snapshot.
	if idx["stopServiceContainers"] >= idx["takeRestoreSnapshot"] {
		t.Errorf("stopServiceContainers (stmt %d) does not run before takeRestoreSnapshot "+
			"(stmt %d); the snapshot is a cross-device copy-and-unlink, so with the service still "+
			"writing it leaves a torn copy in /root/.snapshot and an empty volume",
			idx["stopServiceContainers"], idx["takeRestoreSnapshot"])
	}

	// 5. A failure to stop halts the restore. Calling and ignoring the result is the same
	//    as not calling it at all.
	ifStmt, ok := stopStmt.(*ast.IfStmt)
	if !ok {
		t.Fatalf("the stopServiceContainers call is a %T, not an `if !stopServiceContainers(…)`; "+
			"its false return means the service is half down and must halt the restore before "+
			"anything touches the volume", stopStmt)
	}
	unary, ok := ifStmt.Cond.(*ast.UnaryExpr)
	if !ok || unary.Op != token.NOT {
		t.Fatalf("stopServiceContainers is not guarded by `if !…`; a restore that could not " +
			"quiesce the service must not begin")
	}
	if call, ok := unary.X.(*ast.CallExpr); !ok {
		t.Fatalf("the `if !…` condition in preRestore is not a call to stopServiceContainers")
	} else if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "stopServiceContainers" {
		t.Fatalf("the `if !…` condition in preRestore does not call stopServiceContainers directly")
	}
	if len(ifStmt.Body.List) != 1 {
		t.Fatalf("the stopServiceContainers guard body has %d statements, want exactly "+
			"`return false`; anything else risks continuing into the snapshot with the service "+
			"only partly stopped", len(ifStmt.Body.List))
	}
	ret, ok := ifStmt.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		t.Fatalf("the stopServiceContainers guard does not `return false`; a failed stop that " +
			"falls through takes the snapshot anyway, which is the data-loss path")
	}
	if id, ok := ret.Results[0].(*ast.Ident); !ok || id.Name != "false" {
		t.Fatalf("the stopServiceContainers guard returns something other than false; preRestore's " +
			"caller gates the whole restore on this bool")
	}
	if ifStmt.Else != nil {
		t.Errorf("the stopServiceContainers guard has an else branch; the only correct response " +
			"to a failed stop is to halt")
	}

	// The AST assertions above are all about WHERE the call sits. They would every one of
	// them still pass if stopServiceContainers itself were gutted to `return true`, which
	// would restore the original bug while leaving preRestore looking correct. This is a
	// substring check in the style of TestBackupPathUsesBackupContinueOnError because it is
	// a question about text: the function has to still name the two operations that make it
	// a stop at all.
	src, readErr := os.ReadFile(file)
	if readErr != nil {
		t.Fatalf("read %s: %v", file, readErr)
	}
	fn := stopServiceContainersSource(t, string(src))
	if !strings.Contains(fn, "FindAllByService") {
		t.Error("stopServiceContainers no longer calls FindAllByService; with no container list " +
			"it stops nothing and reports success, and the snapshot runs under the live service")
	}
	if !strings.Contains(fn, ".Stop()") {
		t.Error("stopServiceContainers no longer calls Stop() on anything; preRestore would be " +
			"asking a no-op to quiesce the service before it moves /mnt/data aside")
	}
}

// stopServiceContainersSource returns the text of stopServiceContainers' body, so the
// substring checks cannot be satisfied by an unrelated mention of FindAllByService or
// Stop() elsewhere in the file — preRestore's own comments name both.
func stopServiceContainersSource(t *testing.T, src string) string {
	t.Helper()
	const decl = "func stopServiceContainers("
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatalf("no %s declaration found; preRestore's stop step has been renamed or removed", decl)
	}
	rest := src[start:]
	// The declaration ends at the first line that is exactly a closing brace, gofmt having
	// indented every brace inside the body.
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}
