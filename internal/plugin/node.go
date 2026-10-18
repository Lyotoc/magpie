// Node: the runtime magpie runs plugins on where Bun has no build. Bun
// was never built for 32-bit ARM; its host.js speaks plain ESM plus a
// handful of Bun globals, which node-shim.cjs stands up on Node. The same
// Node carries npm, which installs the plugins where magpie would use
// `bun add`.

package plugin

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/netproxy"
)

// NodeVersion is the Node magpie downloads where Bun has no build. It is
// the newest LTS line with an official linux-armv7l build; Node 24 dropped
// 32-bit ARM, so raising this past 22 means the arm builds stop. Update
// nodeSums and testenv.MinNode when it changes.
const NodeVersion = "22.23.3"

// nodeSums are NodeVersion's archives' SHA-256s, from its SHASUMS256.txt,
// so the download can be taken from a mirror when nodejs.org can't be
// reached; the checksum itself is always asked of nodejs.org. Other
// versions, and MAGPIE_NODE machines, read it from the release every time.
var nodeSums = map[string]string{
	"node-v22.23.3-linux-armv7l.tar.gz": "8b99058ec6f4e372b9887f8ecb577a292adb132d59e5104356428a758fdb126c",
}

// UndiciVersion is the undici the shim builds fetch dispatchers with on a
// Node that has no node:undici module (Node 22): the {proxy} option
// host.js and the plugins pass to fetch is Bun's, and only an undici
// ProxyAgent turns it into a routed request. It is a package of plain JS
// with no dependencies, downloaded once beside the Node.
const UndiciVersion = "8.12.2"

// undiciIntegrity is undici's npm tarball sha512, its registry integrity.
// A var for tests.
var undiciIntegrity = "sha512-XtuDWVdqTLr3NAxm2ly9/hU6kUsJOgGh1RASzMBbsw6aWCOaQzHdW+QQDfdERjfJ2V5vL5pRPa4RetO9UfivDg=="

// nodeRelease and undiciRelease are where the builds come from; vars for
// tests, and mirrors through source.Do like Bun's release.
var (
	nodeRelease   = "https://nodejs.org/dist"
	undiciRelease = "https://registry.npmjs.org/undici/-"
)

var nodeMu sync.Mutex

// useNode is whether plugins run on Node here: where Bun has no build for
// this machine — 32-bit ARM above all — and wherever MAGPIE_PLUGIN_RUNTIME
// says to. A var for tests.
var useNode = func() bool {
	switch os.Getenv("MAGPIE_PLUGIN_RUNTIME") {
	case "node":
		return true
	case "bun":
		return false
	}
	_, err := bunTarget()
	return err != nil
}

// RuntimeName is what the page and the CLI call the runtime plugins run
// on here.
func RuntimeName() string {
	if useNode() {
		return "Node"
	}
	return "Bun"
}

// HasRuntime is whether the runtime plugins need is already at hand,
// without a download.
func HasRuntime() bool {
	if useNode() {
		return HasNode()
	}
	return HasBun()
}

// RuntimeInUse is the version string the page shows for the runtime
// plugins run on here.
func RuntimeInUse() string {
	if useNode() {
		return NodeInUse()
	}
	return BunInUse()
}

// nodeTargetFor is Node's build name for a machine: linux-armv7l for
// 32-bit ARM, where Node is still published while Bun was never built.
func nodeTargetFor(goos, goarch string) (string, error) {
	switch goos {
	case "linux":
		switch goarch {
		case "arm":
			return "linux-armv7l", nil
		case "arm64":
			return "linux-arm64", nil
		case "amd64":
			return "linux-x64", nil
		}
	case "darwin":
		switch goarch {
		case "arm64":
			return "darwin-arm64", nil
		case "amd64":
			return "darwin-x64", nil
		}
	case "windows":
		switch goarch {
		case "amd64":
			return "win-x64", nil
		case "arm64":
			return "win-arm64", nil
		}
	}
	return "", fmt.Errorf("Node has no build for %s/%s", goos, goarch)
}

func nodeTarget() (string, error) { return nodeTargetFor(runtime.GOOS, runtime.GOARCH) }

// nodeArchive is the file of a target's download: Node ships Windows as a
// zip and everything else as a tar.gz.
func nodeArchive(target string) string {
	if strings.HasPrefix(target, "win-") {
		return "node-v" + NodeVersion + "-" + target + ".zip"
	}
	return "node-v" + NodeVersion + "-" + target + ".tar.gz"
}

func nodeChecksum(version, file string) string {
	if version != NodeVersion {
		return ""
	}
	return nodeSums[file]
}

func nodeExe() string {
	if runtime.GOOS == "windows" {
		return "magpie-node.exe"
	}
	return "magpie-node"
}

// nodeRoot and nodeDirOf keep the Nodes, their npm and their undici: like
// bunRoot, under the cache; named magpie-node for the same PROCESS-NAME
// firewall rules as magpie-bun (#1048).
func nodeRoot() string { return filepath.Join(filepath.Dir(catalog.CachePath()), "node") }

func nodeDirOf(v string) string { return filepath.Join(nodeRoot(), v) }

func nodeExeOf(v string) string { return filepath.Join(nodeDirOf(v), "bin", nodeExe()) }

// nodeNpmCLI is the npm magpie runs plugin installs with: the one inside
// the Node it downloaded (unpacked from its tarball or zip). With a
// MAGPIE_NODE of the user's, MAGPIE_NPM names it; npm sits beside a system
// install where it can be found, and where it can't magpie says so rather
// than guessing at another node's npm.
func nodeNpmCLI() (string, error) {
	if p := os.Getenv("MAGPIE_NPM"); p != "" {
		return p, nil
	}
	v := NodeVersion
	if n := os.Getenv("MAGPIE_NODE"); n != "" {
		dir := filepath.Dir(n)
		if filepath.Base(dir) == "bin" {
			dir = filepath.Dir(dir)
		}
		for _, p := range nodeNpmIn(dir) {
			return p, nil
		}
		return "", errors.New("magpie found no npm beside $MAGPIE_NODE; set MAGPIE_NPM to npm-cli.js, or install Node with its npm")
	}
	for _, p := range nodeNpmIn(nodeDirOf(v)) {
		return p, nil
	}
	return "", fmt.Errorf("the Node magpie downloaded has no npm in it: reinstall it by removing %s", nodeDirOf(v))
}

func nodeNpmIn(dir string) []string {
	var found []string
	for _, p := range []string{
		filepath.Join(dir, "lib", "node_modules", "npm", "bin", "npm-cli.js"),
		filepath.Join(dir, "node_modules", "npm", "bin", "npm-cli.js"),
	} {
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
		}
	}
	return found
}

func haveNode(v string) bool {
	_, err := os.Stat(nodeExeOf(v))
	return err == nil
}

// Node is the node to run plugins with: $MAGPIE_NODE when set, else the
// one magpie downloaded with its npm, downloading them now when there is
// none. The undici the fetch proxy needs is downloaded with it; a host
// without it refuses proxied fetches loudly, never quietly direct.
func Node(ctx context.Context) (string, error) {
	if n := os.Getenv("MAGPIE_NODE"); n != "" {
		return n, nil
	}
	nodeMu.Lock()
	defer nodeMu.Unlock()
	if !haveNode(NodeVersion) {
		target, err := nodeTarget()
		if err != nil {
			return "", err
		}
		if err := downloadNode(ctx, NodeVersion, target); err != nil {
			return "", fmt.Errorf("downloading Node %s to run plugins: %w", NodeVersion, err)
		}
	}
	if err := ensureUndici(ctx); err != nil {
		// the host runs without it and refuses proxied fetches; an install
		// that can't reach the registry fails loudly either way
		log.Printf("plugins: undici for the Node host's proxied fetches didn't download: %v; fetches through a provider's proxy will be refused until it can", err)
	}
	return nodeExeOf(NodeVersion), nil
}

func HasNode() bool {
	if os.Getenv("MAGPIE_NODE") != "" {
		return true
	}
	nodeMu.Lock()
	defer nodeMu.Unlock()
	return haveNode(NodeVersion)
}

// NodeInUse is the version the page shows: $MAGPIE_NODE's own word when
// set, asked once; else NodeVersion.
var (
	nodeInUseMu  sync.Mutex
	nodeInUseGot string
)

func NodeInUse() string {
	if n := os.Getenv("MAGPIE_NODE"); n != "" {
		nodeInUseMu.Lock()
		defer nodeInUseMu.Unlock()
		if nodeInUseGot == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if out, err := command(ctx, n, "--version").Output(); err == nil {
				nodeInUseGot = strings.TrimSpace(string(out))
			} else {
				nodeInUseGot = "unknown"
			}
		}
		return nodeInUseGot
	}
	return NodeVersion
}

func downloadNode(ctx context.Context, version, target string) error {
	file := nodeArchive(target)
	base := nodeRelease + "/v" + version + "/"
	want := nodeChecksum(version, file)
	if want == "" {
		sums, err := getURLOfficial(ctx, base+"SHASUMS256.txt", 1<<20)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(bytes.NewReader(sums))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 2 && f[1] == file {
				want = f[0]
			}
		}
		if want == "" {
			return fmt.Errorf("%s isn't in the release's checksums", file)
		}
	}
	b, err := getURL(ctx, base+file, 300<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != want {
		return errors.New("the download's checksum doesn't match the release's")
	}
	dir := nodeDirOf(version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if strings.HasSuffix(file, ".zip") {
		return unpackNodeZip(b, dir)
	}
	return unpackNodeTarGz(b, dir)
}

// unpackNode takes the node binary and npm's package out of the archive:
// the bin/node named magpie-node and everything under the npm tree, no
// headers, man pages or corepack. Member names are checked to stay under
// the archive's own top folder.
func unpackNodeTarGz(b []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			continue
		}
		p, ok := nodeMember(h.Name)
		if !ok {
			continue
		}
		if err := writeFileFrom(dir, p, h.FileInfo().IsDir(), fs.FileMode(h.Mode), tr); err != nil {
			return err
		}
	}
}

func unpackNodeZip(b []byte, dir string) error {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		p, ok := nodeMember(f.Name)
		if !ok {
			continue
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(p.dest)), 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFileFrom(dir, p, false, f.Mode(), rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// nodeMember maps an archive member to where magpie keeps it: the node
// binary becomes magpie-node, npm's package tree is kept whole, everything
// else (corepack, headers, docs, symlinks that point at it all) is left.
type nodeDest struct {
	dest string
	exe  bool
}

func nodeMember(name string) (nodeDest, bool) {
	clean := path.Clean("/" + strings.TrimPrefix(name, "./"))[1:]
	if strings.Contains(clean, "..") {
		return nodeDest{}, false
	}
	parts := strings.Split(clean, "/")
	// the tar has one top folder, the zip hasn't; drop it when it is Node's
	if len(parts) > 1 && strings.HasPrefix(parts[0], "node-v") {
		parts = parts[1:]
	}
	switch {
	case len(parts) == 2 && (parts[0] == "bin") && (parts[1] == "node" || parts[1] == "node.exe"):
		return nodeDest{dest: filepath.Join("bin", filepath.Base(nodeExe())), exe: true}, true
	case len(parts) == 1 && (parts[0] == "node.exe"): // the zip's layout
		return nodeDest{dest: nodeExe(), exe: true}, true
	}
	for i := range parts {
		if parts[i] == "node_modules" && i+1 < len(parts) && parts[i+1] == "npm" && len(parts) > i+2 {
			return nodeDest{dest: strings.Join(parts, "/")}, true
		}
	}
	return nodeDest{}, false
}

func writeFileFrom(dir string, p nodeDest, isDir bool, mode fs.FileMode, r io.Reader) error {
	full := filepath.Join(dir, filepath.FromSlash(p.dest))
	if isDir {
		return os.MkdirAll(full, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := full + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(mode)|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if p.exe || runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, mode|0o600)
	}
	return os.Rename(tmp, full)
}

// nodeHostError says why a downloaded Node couldn't start. Node's builds
// are glibc-linked; on a musl system (an Alpine container, or an HAOS
// add-on one, without the host's glibc) the loader node names isn't
// there, and exec says "no such file or directory" for a file that is.
func nodeHostError(node string, err error) error {
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if b := os.Getenv("MAGPIE_NODE"); b != "" && b == node {
		return fmt.Errorf("this system can't start $MAGPIE_NODE (%s), though it is there: it is likely linked against a C library this system doesn't have (glibc or musl): %w", node, err)
	}
	target, _ := nodeTarget()
	return fmt.Errorf("this system can't start the Node magpie downloaded (%s, %s), though it is there: Node's builds need glibc, which a musl system hasn't. Set MAGPIE_NODE to a node built for this system (on Alpine: apk add nodejs), or run plugins where glibc is: %w", target, node, err)
}

// undiciDir is where the shim's fetch dispatcher library is kept, beside
// the Nodes.
func undiciDir() string { return filepath.Join(nodeRoot(), "undici", UndiciVersion) }

func ensureUndici(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(undiciDir(), "package", "package.json")); err == nil {
		return nil
	}
	b, err := getURL(ctx, undiciRelease+"/undici-"+UndiciVersion+".tgz", 64<<20)
	if err != nil {
		return err
	}
	sum := sha512.Sum512(b)
	if got := "sha512-" + base64.StdEncoding.EncodeToString(sum[:]); got != undiciIntegrity {
		return errors.New("the undici download's checksum doesn't match the registry's")
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer zr.Close()
	dir := undiciDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(h.Name, "package/") {
			continue
		}
		name := filepath.Join(dir, "package", filepath.FromSlash(strings.TrimPrefix(h.Name, "package/")))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(name, mustRead(tr), 0o644); err != nil {
			return err
		}
	}
}

func mustRead(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}

// nodeShimFile writes the shim where the runtime can require it, named by
// its content like the host script is.
func nodeShimFile() (string, error) { return hostScript("shim", nodeShimJS, ".cjs") }

// hostCommand runs the host script on the runtime exe magpie picked: the
// bun host runs `bun run host.js`; on Node the host is a .mjs and the
// shim is required in first, with the undici for its fetch dispatchers
// named.
func hostCommand(ctx context.Context, exe, dir, js string) (*exec.Cmd, error) {
	if !useNode() {
		return bunCommand(ctx, exe, dir, "run", js), nil
	}
	shim, err := nodeShimFile()
	if err != nil {
		return nil, err
	}
	cmd := command(ctx, exe, "--require", shim, js)
	cmd.Dir = dir
	cmd.Env = append(netproxy.EnvForBun(env()), caEnv(env(), dir)...)
	if u := filepath.Join(undiciDir(), "package"); dirExists(u) {
		cmd.Env = append(cmd.Env, "MAGPIE_SHIM_UNDICI="+u)
	}
	return cmd, nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// npmCommand runs npm from the Node in use, its cache beside the Nodes,
// its registry mirror and the system trust roots the way bunCommand sets
// them for Bun. npm takes the proxy environment the same env vars read.
var npmCommand = func(ctx context.Context, node, npm, dir string, args ...string) *exec.Cmd {
	cmd := command(ctx, node, append([]string{npm}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(netproxy.EnvForBun(env()), "NPM_CONFIG_CACHE="+filepath.Join(nodeRoot(), "install-cache"))
	cmd.Env = append(cmd.Env, caEnv(cmd.Env, dir)...)
	cmd.Env = append(cmd.Env, registryEnvNpm(cmd.Env)...)
	return cmd
}

// pkgRT is the package manager magpie changes plugins/ with: Bun's own
// add/update/remove, or the npm inside the Node magpie runs plugins on.
// The two spell their three operations differently; what they share — the
// plugins folder, the one-at-a-time lock, the environment — stays in the
// callers, as it was with bun.
type pkgRT struct {
	node bool
	exe  string
	npm  string // npm-cli.js; "" on Bun
}

// resolvePkg picks and prepares the runtime for package work, downloading
// what it needs. It does not take installMu; callers hold it as before.
func resolvePkg(ctx context.Context) (pkgRT, error) {
	if !useNode() {
		bun, err := Bun(ctx)
		return pkgRT{exe: bun}, err
	}
	node, err := Node(ctx)
	if err != nil {
		return pkgRT{}, err
	}
	npm, err := nodeNpmCLI()
	if err != nil {
		return pkgRT{}, err
	}
	return pkgRT{node: true, exe: node, npm: npm}, nil
}

// pkgLabel is how magpie names a package command in a message: "bun add",
// "npm install", and their like.
func pkgLabel(action string) string {
	if useNode() {
		return map[string]string{"add": "npm install", "remove": "npm uninstall", "update": "npm update"}[action]
	}
	return "bun " + action
}

// addCmd installs spec into plugins/ with its scripts left unrun, as
// OpenCode leaves them.
func (r pkgRT) addCmd(ctx context.Context, spec string) *exec.Cmd {
	if r.node {
		return npmCommand(ctx, r.exe, r.npm, Dir(), "install", "--ignore-scripts", "--no-audit", "--no-fund", spec)
	}
	return bunCommand(ctx, r.exe, Dir(), "add", "--ignore-scripts", spec)
}

// removeCmd takes the package named out of plugins/.
func (r pkgRT) removeCmd(ctx context.Context, name string) *exec.Cmd {
	if r.node {
		return npmCommand(ctx, r.exe, r.npm, Dir(), "uninstall", "--ignore-scripts", name)
	}
	return bunCommand(ctx, r.exe, Dir(), "remove", "--ignore-scripts", name)
}

// updateCmd refreshes an installed package. On Bun `bun update <name>`
// re-fetches a git dependency's branch tip; npm's update won't move a git
// dependency, so a git one (which has a spec to re-resolve) goes through
// `npm install <name>@<spec> --force`, which takes the ref again now.
func (r pkgRT) updateCmd(ctx context.Context, name, spec string) *exec.Cmd {
	if r.node && spec != "" && spec != name {
		return npmCommand(ctx, r.exe, r.npm, Dir(), "install", "--ignore-scripts", "--force", name+"@"+spec)
	}
	if r.node {
		return npmCommand(ctx, r.exe, r.npm, Dir(), "update", "--ignore-scripts", name)
	}
	return bunCommand(ctx, r.exe, Dir(), "update", "--ignore-scripts", name)
}

// pkgAdd adds spec to plugins/, on either runtime. installMu held.
func pkgAdd(ctx context.Context, r pkgRT, spec string) error {
	out, err := r.addCmd(ctx, spec).CombinedOutput()
	if err == nil {
		return nil
	}
	if !r.node {
		if hint := missingLibs(string(out)); hint != "" {
			return fmt.Errorf("%s %s: %s: %w", pkgLabel("add"), spec, hint, err)
		}
		return fmt.Errorf("%s %s: %v: %s", pkgLabel("add"), spec, cantStart(r.exe, err), lastLines(string(out), 6))
	}
	return fmt.Errorf("%s %s: %v: %s", pkgLabel("add"), spec, nodeHostError(r.exe, err), lastLines(string(out), 6))
}
