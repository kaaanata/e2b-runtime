//go:build linux

package sandbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// The checkpoint protocol is an ORDER of side effects inside functions that
// drive a real Firecracker process (Pause, CreateSandbox, ResumeSandbox, and
// fc.Process.Create/Resume), which unit tests cannot run end to end. These
// tests pin the order in the source itself: each named step must appear, in
// the given sequence, in the function's own body — calls inside closures
// (cleanup handlers such as the resume-on-error rollback) do not count, so a
// rollback resume can never be mistaken for the success-path resume.

// stepPositions returns, for each name, the position of its first occurrence
// in fn's body outside function literals. A name is "recv.Method" for a call
// whose selector ends in that pair (e.g. "process.CreateSnapshot"), a bare
// "Func" for a plain call, "Func(arg)" for a call with one identifier
// argument, or "assign:<lhs>=<rhs>" for an assignment.
func stepPositions(t *testing.T, fn *ast.FuncDecl, names []string) map[string]token.Pos {
	t.Helper()

	found := map[string]token.Pos{}
	record := func(name string, pos token.Pos) {
		if _, ok := found[name]; !ok {
			found[name] = pos
		}
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			// A single identifier argument is part of the name too, so
			// linkWorkspaceDrives(hostPath) is told apart from (devNull).
			arg := ""
			if len(n.Args) == 1 {
				if id, ok := n.Args[0].(*ast.Ident); ok {
					arg = "(" + id.Name + ")"
				}
			}

			switch f := n.Fun.(type) {
			case *ast.SelectorExpr:
				record(f.Sel.Name, n.Pos())
				if arg != "" {
					record(f.Sel.Name+arg, n.Pos())
				}
				switch x := f.X.(type) {
				case *ast.Ident:
					record(x.Name+"."+f.Sel.Name, n.Pos())
				case *ast.SelectorExpr:
					record(x.Sel.Name+"."+f.Sel.Name, n.Pos())
				}
			case *ast.Ident:
				record(f.Name, n.Pos())
			}
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
				lhs, okL := n.Lhs[0].(*ast.Ident)
				rhs, okR := n.Rhs[0].(*ast.Ident)
				if okL && okR {
					record("assign:"+lhs.Name+"="+rhs.Name, n.Pos())
				}
			}
		}

		return true
	})

	for _, name := range names {
		require.Contains(t, found, name, "%s: step %q not found in the function body", fn.Name.Name, name)
	}

	return found
}

func requireOrder(t *testing.T, file, recv, fnName string, steps ...string) {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	require.NoError(t, err)

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fnName {
			continue
		}

		if recv == "" && fd.Recv == nil {
			fn = fd
		}

		if recv != "" && fd.Recv != nil && len(fd.Recv.List) == 1 {
			if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
				if id, ok := star.X.(*ast.Ident); ok && id.Name == recv {
					fn = fd
				}
			}
		}
	}
	require.NotNil(t, fn, "%s.%s not found in %s", recv, fnName, file)

	pos := stepPositions(t, fn, steps)
	for i := 1; i < len(steps); i++ {
		require.Less(t, pos[steps[i-1]], pos[steps[i]], "%s: %q must come before %q (%s vs %s)",
			fnName, steps[i-1], steps[i], fset.Position(pos[steps[i-1]]), fset.Position(pos[steps[i]]))
	}
}

// TestPauseOrder_CreateSnapshotSealResume pins §6.5/§7.1: the workspace seal
// runs after CreateSnapshot drained and fsynced the drives, the metadata is
// stamped with its revisions before it is written, and the in-place resume is
// reachable only through the sealed-guard, which itself precedes clearing the
// resume-on-error rollback.
func TestPauseOrder_CreateSnapshotSealResume(t *testing.T) {
	t.Parallel()

	requireOrder(t, "sandbox.go", "Sandbox", "Pause",
		"checkPauseSupported",
		"process.Pause",
		"process.CreateSnapshot",
		"sealWorkspaceForPause",
		"stampWorkspaceMetadata",
		"m.ToFile",
		"requireWorkspaceSealed",
		"assign:resumeOnError=false",
		"process.ResumeInPlace",
	)

	// The filesystem-only path freezes the whole mount set before the FC pause.
	requireOrder(t, "sandbox.go", "Sandbox", "Pause",
		"guestFreezeMountSetForPause",
		"process.Pause",
	)
}

func TestCreateSandboxOrder_ColdBootWorkspace(t *testing.T) {
	t.Parallel()

	requireOrder(t, "sandbox.go", "Factory", "CreateSandbox",
		"planSandboxWorkspace",
		"prepareColdBoot",
		"fc.NewProcess",
		"installWorkspace",
		"fcHandle.Create",
	)
}

func TestResumeSandboxOrder_Workspace(t *testing.T) {
	t.Parallel()

	requireOrder(t, "sandbox.go", "Factory", "ResumeSandbox",
		"planSandboxWorkspace",
		"fc.NewProcess",
		"installWorkspace",
		"fcHandle.Resume",
		"WaitForEnvd",
		"resizeWorkspaceFilesystems",
	)
}

// TestFCProcessOrder_Workspace pins the fc side: admission (Bind with the pid)
// before any slot link is re-pointed from /dev/null to the volume, and, on a
// resume, the size refresh PATCH after the snapshot load and before the guest
// runs again.
func TestFCProcessOrder_Workspace(t *testing.T) {
	t.Parallel()

	requireOrder(t, "fc/process.go", "Process", "Create",
		"linkWorkspaceDrives(devNull)",
		"configure",
		"admitWorkspace",
		"linkWorkspaceDrives(hostPath)",
		"setWorkspaceDrive",
	)

	requireOrder(t, "fc/process.go", "Process", "Resume",
		"linkWorkspaceDrives(devNull)",
		"admitWorkspace",
		"linkWorkspaceDrives(hostPath)",
		"loadSnapshot",
		"patchDrivePath",
		"resumeVM",
	)
}
