//go:build linux

package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectCheckpointPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		workspace    bool
		syncWP       bool
		flag         bool
		fc           bool
		wantInPlace  bool
		wantErr      error
		wantFlagRead bool
	}{
		// Plain sandboxes: today's gate, unchanged.
		{name: "plain/all on", syncWP: true, flag: true, fc: true, wantInPlace: true, wantFlagRead: true},
		{name: "plain/async WP", flag: true, fc: true},
		{name: "plain/flag off", syncWP: true, fc: true, wantFlagRead: true},
		{name: "plain/old fc", syncWP: true, flag: true, wantFlagRead: true},
		// Workspace sandboxes: in place whenever the FC contract has it,
		// regardless of the flag and of use_sync_wp (the Citadel default:
		// both off), never resume-fresh.
		{name: "workspace/flags off, async WP", workspace: true, fc: true, wantInPlace: true},
		{name: "workspace/all on", workspace: true, syncWP: true, flag: true, fc: true, wantInPlace: true},
		{name: "workspace/old fc", workspace: true, syncWP: true, flag: true, wantErr: errInPlaceCheckpointUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			flagRead := false
			inPlace, err := selectCheckpointPath(tc.workspace, tc.syncWP,
				func() bool {
					flagRead = true

					return tc.flag
				},
				func() bool { return tc.fc },
			)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.False(t, inPlace)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantInPlace, inPlace)
			assert.Equal(t, tc.wantFlagRead, flagRead, "flag evaluation must keep the short-circuit order")
		})
	}
}

// findFunc returns the method recv.name (recv "" for a plain function) of
// sandboxes.go.
func findFunc(t *testing.T, fset *token.FileSet, recv, name string) *ast.FuncDecl {
	t.Helper()

	f, err := parser.ParseFile(fset, "sandboxes.go", nil, 0)
	require.NoError(t, err)

	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name {
			continue
		}
		if recv == "" && fd.Recv == nil {
			return fd
		}
		if recv != "" && fd.Recv != nil && len(fd.Recv.List) == 1 {
			if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
				if id, ok := star.X.(*ast.Ident); ok && id.Name == recv {
					return fd
				}
			}
		}
	}
	require.Failf(t, "function not found", "%s.%s", recv, name)

	return nil
}

// calls returns the first position of each called selector/function name in
// fn's body outside function literals, plus the call expressions by name.
func calls(fn *ast.FuncDecl) (map[string]token.Pos, map[string]*ast.CallExpr) {
	pos := map[string]token.Pos{}
	exprs := map[string]*ast.CallExpr{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			var name string
			switch f := n.Fun.(type) {
			case *ast.SelectorExpr:
				name = f.Sel.Name
			case *ast.Ident:
				name = f.Name
			}
			if _, ok := pos[name]; name != "" && !ok {
				pos[name] = n.Pos()
				exprs[name] = n
			}
		}

		return true
	})

	return pos, exprs
}

// TestCheckpointOrder_WorkspaceInPlace pins the server half of the in-place
// checkpoint for a sandbox with workspace volumes (the Pause half — Pause →
// CreateSnapshot → seal → ResumeInPlace — is pinned by
// sandbox.TestPauseOrder_CreateSnapshotSealResume): Checkpoint selects the
// path (refusing before anything destructive), the in-place branch takes the
// in-flight guard and snapshots with maintainSandbox=true, resumeFresh=false,
// and snapshotAndCacheSandbox turns that into WithMaintainSandbox on Pause.
func TestCheckpointOrder_WorkspaceInPlace(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	ckpt, _ := calls(findFunc(t, fset, "Server", "Checkpoint"))
	for _, step := range []string{"selectCheckpointPath", "CheckPauseSupported", "checkpointInPlace", "checkpointResumeFresh"} {
		require.Contains(t, ckpt, step)
	}
	assert.Less(t, ckpt["selectCheckpointPath"], ckpt["CheckPauseSupported"])
	assert.Less(t, ckpt["CheckPauseSupported"], ckpt["checkpointInPlace"])
	assert.Less(t, ckpt["selectCheckpointPath"], ckpt["checkpointResumeFresh"])

	inPlace, exprs := calls(findFunc(t, fset, "Server", "checkpointInPlace"))
	require.Contains(t, inPlace, "BeginInPlaceCheckpoint")
	require.Contains(t, inPlace, "snapshotAndCacheSandbox")
	assert.Less(t, inPlace["BeginInPlaceCheckpoint"], inPlace["snapshotAndCacheSandbox"])
	assert.NotContains(t, inPlace, "MarkStopping", "an in-place checkpoint keeps the sandbox live")

	args := exprs["snapshotAndCacheSandbox"].Args
	require.GreaterOrEqual(t, len(args), 2)
	maintain, ok := args[len(args)-2].(*ast.Ident)
	require.True(t, ok)
	resumeFresh, ok := args[len(args)-1].(*ast.Ident)
	require.True(t, ok)
	assert.Equal(t, "true", maintain.Name, "maintainSandbox")
	assert.Equal(t, "false", resumeFresh.Name, "resumeFresh")

	snap, _ := calls(findFunc(t, fset, "Server", "snapshotAndCacheSandbox"))
	require.Contains(t, snap, "WithMaintainSandbox")
	require.Contains(t, snap, "Pause")
	assert.Less(t, snap["WithMaintainSandbox"], snap["Pause"])
}
