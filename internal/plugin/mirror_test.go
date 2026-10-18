package plugin

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/source"
)

// with 「国内镜像」, bun installs from npmmirror, unless the environment
// names a registry of its own
func TestBunInstallsFromChinaMirror(t *testing.T) {
	was := source.China
	t.Cleanup(func() { source.China = was })
	t.Setenv("BUN_CONFIG_REGISTRY", "")
	t.Setenv("NPM_CONFIG_REGISTRY", "")
	reg := func() []string {
		var out []string
		for _, kv := range bunCommand(context.Background(), "bun", t.TempDir(), "add", "x").Env {
			if k, _, _ := strings.Cut(kv, "="); k == "BUN_CONFIG_REGISTRY" && kv != "BUN_CONFIG_REGISTRY=" {
				out = append(out, kv)
			}
		}
		return out
	}
	source.China = func() bool { return false }
	if r := reg(); len(r) != 0 {
		t.Fatalf("switch off: %q", r)
	}
	source.China = func() bool { return true }
	if r := reg(); !slices.Equal(r, []string{"BUN_CONFIG_REGISTRY=https://registry.npmmirror.com/"}) {
		t.Fatalf("switch on: %q", r)
	}
	t.Setenv("NPM_CONFIG_REGISTRY", "https://npm.example/")
	if r := reg(); len(r) != 0 {
		t.Fatalf("a registry of the user's own: %q", r)
	}
}

// the same, for npm on the Node runtime: it names its registry with
// npm_config_registry, and not with Bun's variable
func TestNpmInstallsFromChinaMirror(t *testing.T) {
	was := source.China
	t.Cleanup(func() { source.China = was })
	t.Setenv("BUN_CONFIG_REGISTRY", "")
	t.Setenv("NPM_CONFIG_REGISTRY", "")
	npmEnv := func() []string {
		cmd := npmCommand(context.Background(), "node", "npm-cli.js", t.TempDir(), "install", "x")
		var out []string
		for _, kv := range cmd.Env {
			k, v, _ := strings.Cut(kv, "=")
			if strings.ToUpper(k) == "NPM_CONFIG_REGISTRY" && v != "" {
				out = append(out, kv)
			}
		}
		return out
	}
	source.China = func() bool { return false }
	if r := npmEnv(); len(r) != 0 {
		t.Fatalf("switch off: %q", r)
	}
	source.China = func() bool { return true }
	if r := npmEnv(); !slices.Contains(r, "NPM_CONFIG_REGISTRY=https://registry.npmmirror.com/") {
		t.Fatalf("switch on: %q", r)
	}
	t.Setenv("NPM_CONFIG_REGISTRY", "https://npm.example/")
	if r := npmEnv(); !slices.Equal(r, []string{"NPM_CONFIG_REGISTRY=https://npm.example/"}) {
		t.Fatalf("a registry of the user's own, kept as it is: %q", r)
	}
}
