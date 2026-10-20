package plugin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

// nodeSandbox points magpie's folders at a temp home, as the plugin tests
// do, so a download lands in the test and not the machine.
func nodeSandbox(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("MAGPIE_PLUGIN_RUNTIME", "")
	t.Setenv("MAGPIE_NODE", "")
	t.Setenv("MAGPIE_NPM", "")
}

func TestUseNodeWhereBunHasNoBuild(t *testing.T) {
	old := bunPlatform
	defer func() { bunPlatform = old }()
	nodeSandbox(t)

	bunPlatform = func() (string, string, bool) { return "linux", "arm", false }
	if !useNode() {
		t.Fatal("32-bit ARM has no Bun build; plugins must run on Node")
	}
	bunPlatform = func() (string, string, bool) { return "linux", "arm64", false }
	if useNode() {
		t.Fatal("arm64 has a Bun build; Node must not be chosen")
	}
	t.Setenv("MAGPIE_PLUGIN_RUNTIME", "node")
	bunPlatform = func() (string, string, bool) { return "darwin", "arm64", false }
	if !useNode() {
		t.Fatal("MAGPIE_PLUGIN_RUNTIME=node must hold even where Bun builds")
	}
	t.Setenv("MAGPIE_PLUGIN_RUNTIME", "bun")
	bunPlatform = func() (string, string, bool) { return "linux", "arm", false }
	if useNode() {
		t.Fatal("MAGPIE_PLUGIN_RUNTIME=bun must hold even where Bun has no build")
	}
}

func TestNodeTargetFor(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"linux", "arm", "linux-armv7l"},
		{"linux", "arm64", "linux-arm64"},
		{"linux", "amd64", "linux-x64"},
		{"darwin", "arm64", "darwin-arm64"},
		{"darwin", "amd64", "darwin-x64"},
		{"windows", "amd64", "win-x64"},
	}
	for _, c := range cases {
		got, err := nodeTargetFor(c.goos, c.goarch)
		if err != nil || got != c.want {
			t.Errorf("nodeTargetFor(%s,%s) = %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
	for _, c := range [][2]string{{"linux", "386"}, {"freebsd", "amd64"}, {"android", "arm"}} {
		if _, err := nodeTargetFor(c[0], c[1]); err == nil {
			t.Errorf("nodeTargetFor(%s,%s) should say there is no build", c[0], c[1])
		}
	}
	if a := nodeArchive("linux-armv7l"); !strings.HasSuffix(a, ".tar.gz") {
		t.Errorf("unix archives are tar.gz, got %s", a)
	}
	if a := nodeArchive("win-x64"); !strings.HasSuffix(a, ".zip") {
		t.Errorf("windows archives are zip, got %s", a)
	}
}

// tarGzNode builds a stand-in for Node's archive: the binary and npm's
// package that magpie must take, plus the headers, docs and symlinks it
// must leave.
func tarGzNode(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	top := "node-v" + NodeVersion + "-linux-armv7l"
	write := func(name, body string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: top + "/" + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	write("bin/node", "#!sh\nnode\n", 0o755)
	write("lib/node_modules/npm/bin/npm-cli.js", "npm\n", 0o644)
	write("lib/node_modules/npm/package.json", "{}\n", 0o644)
	write("include/node/v8.h", "skip me\n", 0o644)
	write("share/doc/node/l.html", "skip me\n", 0o644)
	tw.WriteHeader(&tar.Header{Name: top + "/bin/npm", Typeflag: tar.TypeSymlink, Linkname: "../lib/node_modules/npm/bin/npm-cli.js"})
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestNodeDownloadChecksAndUnpacks(t *testing.T) {
	nodeSandbox(t)
	archive := tarGzNode(t)
	sum := sha256.Sum256(archive)
	file := nodeArchive("linux-armv7l")
	oldSums, oldRelease := nodeSums, nodeRelease
	nodeSums = nil
	defer func() { nodeSums, nodeRelease = oldSums, oldRelease }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "SHASUMS256.txt"):
			fmt.Fprintf(w, "%x  %s\n", sum, file)
			fmt.Fprintf(w, "0000000000000000000000000000000000000000000000000000000000000000  other-thing.tar.gz\n")
		case strings.HasSuffix(r.URL.Path, file):
			w.Write(archive)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	nodeRelease = srv.URL

	ctx := context.Background()
	if err := downloadNode(ctx, NodeVersion, "linux-armv7l"); err != nil {
		t.Fatal(err)
	}
	exe := nodeExeOf(NodeVersion)
	if b, err := os.ReadFile(exe); err != nil || !strings.Contains(string(b), "node") {
		t.Fatalf("magpie-node missing: %v %q", err, b)
	}
	if fi, err := os.Stat(exe); err == nil && fi.Mode()&0o100 == 0 {
		t.Errorf("magpie-node is not executable: %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(nodeDirOf(NodeVersion), "lib", "node_modules", "npm", "bin", "npm-cli.js")); err != nil {
		t.Errorf("npm was not unpacked with the node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nodeDirOf(NodeVersion), "include")); err == nil {
		t.Error("headers should not be unpacked")
	}
	if _, err := os.Stat(filepath.Join(nodeDirOf(NodeVersion), "bin", "npm")); err == nil {
		t.Error("symlinks should be left out")
	}

	// a checksum that doesn't match is refused, and the Node is not left
	// half-extracted for the next start to trust
	tampered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHASUMS256.txt") {
			fmt.Fprint(w, "1111111111111111111111111111111111111111111111111111111111111111  x\n")
			return
		}
		w.Write(archive)
	}))
	defer tampered.Close()
	nodeRelease = tampered.URL
	if err := os.RemoveAll(nodeDirOf(NodeVersion)); err != nil {
		t.Fatal(err)
	}
	if err := downloadNode(ctx, NodeVersion, "linux-armv7l"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a mismatched checksum must be refused: %v", err)
	}
	if haveNode(NodeVersion) {
		t.Error("nothing may be left installed after a refused download")
	}
}

func TestUndiciDownloadChecksItsChecksum(t *testing.T) {
	nodeSandbox(t)
	var body bytes.Buffer
	{
		zw := gzip.NewWriter(&body)
		tw := tar.NewWriter(zw)
		add := func(name, text string) {
			tw.WriteHeader(&tar.Header{Name: "package/" + name, Mode: 0o644, Size: int64(len(text)), Typeflag: tar.TypeReg})
			tw.Write([]byte(text))
		}
		add("package.json", `{"name":"undici","version":"`+UndiciVersion+`","main":"index.js"}`)
		add("index.js", "module.exports = {}")
		tw.Close()
		zw.Close()
	}
	b := body.Bytes()
	sum := sha512.Sum512(b)
	oldRel, oldInt := undiciRelease, undiciIntegrity
	undiciRelease = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(b)
	})).URL
	undiciIntegrity = "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	t.Cleanup(func() { undiciRelease, undiciIntegrity = oldRel, oldInt })

	ctx := context.Background()
	if err := ensureUndici(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(undiciDir(), "package", "package.json")); err != nil {
		t.Fatalf("undici not extracted: %v", err)
	}

	// one whose checksum isn't the registry's is refused and left unseen
	undiciIntegrity = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	if err := os.RemoveAll(undiciDir()); err != nil {
		t.Fatal(err)
	}
	if err := ensureUndici(ctx); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("undici with a wrong checksum must be refused: %v", err)
	}
}

func TestPkgRuntimeCommands(t *testing.T) {
	old := useNode
	defer func() { useNode = old }()

	useNode = func() bool { return false }
	if l := pkgLabel("add"); l != "bun add" {
		t.Errorf("bun label %q", l)
	}
	useNode = func() bool { return true }
	for action, want := range map[string]string{"add": "npm install", "remove": "npm uninstall", "update": "npm update"} {
		if l := pkgLabel(action); l != want {
			t.Errorf("%s label %q want %q", action, l, want)
		}
	}
	r := pkgRT{node: true, exe: "/n/bin/magpie-node", npm: "/n/lib/node_modules/npm/bin/npm-cli.js"}
	ctx := context.Background()
	add := r.addCmd(ctx, "@x/y@1.2.3")
	if add.Args[0] != "/n/bin/magpie-node" || add.Args[1] != r.npm || add.Args[2] != "install" {
		t.Errorf("npm add args %v", add.Args)
	}
	if !strings.Contains(strings.Join(add.Args, " "), "--ignore-scripts") {
		t.Errorf("npm add must leave scripts unrun: %v", add.Args)
	}
	rem := r.removeCmd(ctx, "x")
	if !strings.Contains(strings.Join(rem.Args, " "), "uninstall") {
		t.Errorf("npm remove args %v", rem.Args)
	}
	git := r.updateCmd(ctx, "x", "github:a/b#main")
	joined := strings.Join(git.Args, " ")
	if !strings.Contains(joined, "install") || !strings.Contains(joined, "x@github:a/b#main") || !strings.Contains(joined, "--force") {
		t.Errorf("a git update must re-resolve the spec with --force: %v", git.Args)
	}
	plain := r.updateCmd(ctx, "x", "x")
	if !strings.Contains(strings.Join(plain.Args, " "), "update") {
		t.Errorf("an npm update of an npm package: %v", plain.Args)
	}
	// and the bun shape is unchanged
	b := pkgRT{exe: "/x/magpie-bun"}
	bargs := b.addCmd(ctx, "spec").Args
	if len(bargs) < 4 || bargs[0] != "/x/magpie-bun" || bargs[1] != "add" || bargs[len(bargs)-1] != "spec" {
		t.Errorf("bun add args %v", bargs)
	}
}

// TestNodeShimGlobals runs the embedded shim under the machine's node and
// checks what the host reads from the Bun global, including that a child
// a plugin starts through node:child_process gets the host's guard — the
// patch the shim installs must be what both the CJS require and the ESM
// import of child_process end up calling.
func TestNodeShimGlobals(t *testing.T) {
	nodeSandbox(t)
	node := testenv.Node(t)
	dir := t.TempDir()
	shim := filepath.Join(dir, "shim.cjs")
	if err := os.WriteFile(shim, nodeShimJS, 0o644); err != nil {
		t.Fatal(err)
	}
	// a package to resolve, as pi.js resolves pi
	pd := filepath.Join(dir, "node_modules", "tmpmod")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pd, "package.json"), []byte(`{"name":"tmpmod","main":"m.js"}`), 0o644)
	os.WriteFile(filepath.Join(pd, "m.js"), []byte("x"), 0o644)

	script := fmt.Sprintf(`
(async () => {
  const out = {};
  const $ = Bun.$;
  out.inspect = Bun.inspect({ a: [1, 2] }).replace(/\s+/g, " ");
  out.version = /^\d/.test(Bun.version) ? "ok" : Bun.version;
  try {
    const p = Bun.resolveSync("tmpmod", "%s");
    out.resolve = p.endsWith("m.js") || p.endsWith("m") ? "ok" : p;
  } catch (e) { out.resolve = "ERR " + e.message }
  globalThis.__magpieGuard = (o) => ({ ...o, env: { ...o.env, ...process.env, MARK: "1" } });
  const { spawnSync } = require("node:child_process");
  out.cjsGuard = spawnSync("/bin/sh", ["-c", 'test "$MARK" = 1 && echo guarded'], { encoding: "utf8" }).stdout.trim();
  const esm = await import("node:child_process");
  out.esmGuard = esm.spawnSync("/bin/sh", ["-c", 'test "$MARK" = 1 && echo guarded'], { encoding: "utf8" }).stdout.trim();
  const r = await $("echo shell-works");
  out.dollar = r.text().trim();
  const bad = await $("exit 7").nothrow();
  out.nothrow = String(bad.exitCode);
  const tmpl = ["echo ", ""]; tmpl.raw = tmpl;
  const nested = await $(tmpl, "a b");
  out.quoting = nested.text().trim();
  out.spawn = Bun.spawn ? "ok" : "missing";
  console.log(JSON.stringify(out));
})().catch((e) => { console.log("FATAL " + (e?.stack || e)); process.exit(1) });
`, dir)

	cmd := exec.Command(node, "--require", shim, "-e", script)
	cmd.Env = append(os.Environ(), "MAGPIE_SHIM_UNDICI=") // the no-undici path, if node has no node:undici of its own
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim run: %v\n%s", err, out)
	}
	var got map[string]string
	line := strings.TrimSpace(string(out))
	i := strings.Index(line, "{")
	if i < 0 {
		t.Fatalf("shim printed no JSON: %q", out)
	}
	if err := json.Unmarshal([]byte(line[i:]), &got); err != nil {
		t.Fatalf("shim output %q", out)
	}
	for k, want := range map[string]string{
		"inspect":  "{ a: [ 1, 2 ] }",
		"version":  "ok",
		"resolve":  "ok",
		"cjsGuard": "guarded",
		"esmGuard": "guarded",
		"dollar":   "shell-works",
		"nothrow":  "7",
		"quoting":  "a b",
		"spawn":    "ok",
	} {
		if got[k] != want {
			t.Errorf("shim %s = %q, want %q", k, got[k], want)
		}
	}
}

// TestHostRunsOnNode is the armv7 story end to end: with the runtime
// forced to the machine's node, the real host boots, loads a real-shape
// plugin folder and answers the provider list.
func TestHostRunsOnNode(t *testing.T) {
	nodeSandbox(t)
	node := testenv.Node(t)
	t.Setenv("MAGPIE_PLUGIN_RUNTIME", "node")
	t.Setenv("MAGPIE_NODE", node)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("FAKE_BASE", srv.URL+"/v1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("testdata/fake/index.js")
	if _, err := Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	ls, err := Plugins(ctx)
	if err != nil || len(ls) != 1 || ls[0].Error != "" {
		t.Fatalf("Plugins = %+v, %v", ls, err)
	}
	ps, err := Providers(ctx)
	if err != nil || len(ps) != 1 || ps[0].ID != "fakeco" {
		t.Fatalf("Providers = %+v, %v", ps, err)
	}
	hostMu.Lock()
	h := current
	hostMu.Unlock()
	if h == nil || filepath.Base(h.cmd.Path) != filepath.Base(node) {
		t.Fatalf("the host should run on the node: %+v", h)
	}
}

func TestNodeHostErrorNamesTheLoaderProblem(t *testing.T) {
	old := useNode
	defer func() { useNode = old }()
	dir := t.TempDir()
	exe := filepath.Join(dir, "magpie-node")
	if err := os.WriteFile(exe, []byte("#!/nonexistent/ld-linux-armhf.so.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a missing loader makes exec say "no such file or directory" for a
	// binary that is there: exactly glibc missing on a musl system
	_, err := exec.Command(exe).Output()
	if err == nil {
		t.Skip("this machine runs shell scripts named magpie-node")
	}
	got := nodeHostError(exe, err)
	if !strings.Contains(got.Error(), "glibc") {
		t.Errorf("nodeHostError = %v, want it to name glibc", got)
	}
	if nodeHostError(exe, nil) != nil {
		t.Error("no error stays no error")
	}
	t.Setenv("MAGPIE_NODE", exe)
	if got := nodeHostError(exe, fmt.Errorf("boom: %w", os.ErrNotExist)); !strings.Contains(got.Error(), "MAGPIE_NODE") {
		t.Errorf("$MAGPIE_NODE's word: %v", got)
	}
}
