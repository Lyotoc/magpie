package testenv

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// MinNode is the oldest Node a test may run the plugin host on: magpie's
// own NodeVersion is what it downloads, and these tests borrow the
// machine's. Node 20 is the floor because the shim needs the global fetch
// of 18 and the built-in-module snapshots of child_process patches that
// 20 keeps stable; magpie's download is 22.
const MinNode = "20"

// nodesRun holds a *bunRun-style once-per-binary version for nodes.
var nodesRun sync.Map

// Node is the node on PATH for a test that runs the host on Node (as
// MAGPIE_NODE with MAGPIE_PLUGIN_RUNTIME=node); a machine without one
// skips the test. Like Bun, it runs the binary once first: macOS'
// first-exec check must not eat a test's deadlines.
func Node(t testing.TB) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	r, _ := nodesRun.LoadOrStore(node, new(bunRun))
	run := r.(*bunRun)
	run.once.Do(func() {
		out, _ := exec.Command(node, "--version").Output()
		run.version = strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	})
	if older(run.version, MinNode) {
		t.Skipf("node %s on PATH is older than %s, the oldest these tests run the host on", run.version, MinNode)
	}
	return node
}
