package plugin

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeDist serves the community repo's dist branch, as scripts/dist.mjs
// there writes it, with the versions given of each package (the last one
// named its latest); tamper serves every tarball other than the one its
// index names.
func fakeDist(t *testing.T, pkgs map[string][]string, tamper bool) {
	t.Helper()
	files := map[string][]byte{}
	idx := map[string]any{}
	for name, versions := range pkgs {
		vs := map[string]any{}
		for _, v := range versions {
			tgz := npmTarball(t, name, v)
			file := strings.ReplaceAll(strings.TrimPrefix(name, "@"), "/", "-") + "-" + v + ".tgz"
			sum := sha512.Sum512(tgz)
			if tamper {
				tgz = npmTarball(t, name, v+"-not")
			}
			files["/tgz/"+file] = tgz
			pj, _ := json.Marshal(map[string]string{"name": name, "version": v, "description": "from dist", "author": "dbydd"})
			vs[v] = map[string]any{"file": "tgz/" + file, "integrity": "sha512-" + base64.StdEncoding.EncodeToString(sum[:]), "manifest": json.RawMessage(pj)}
		}
		idx[name] = map[string]any{"latest": versions[len(versions)-1], "readme": "# " + name + " from dist", "versions": vs}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.json" {
			json.NewEncoder(w).Encode(map[string]any{"version": 1, "packages": idx})
			return
		}
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	forgetDist := func() {
		distMu.Lock()
		distIdx, distAt, distOK = nil, time.Time{}, false
		distMu.Unlock()
	}
	base := distBase
	distBase = srv.URL
	forgetDist()
	t.Cleanup(func() { distBase = base; forgetDist() })
}

// npmHasNothing is an npm registry that has no package at all: a new
// plugin, merged before anyone could publish it there.
func npmHasNothing(t *testing.T) {
	fakeNPM(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			w.Write([]byte(`{"downloads":0}`))
			return
		}
		http.NotFound(w, r)
	})
	t.Setenv("BUN_CONFIG_REGISTRY", npmRegistry+"/")
}

// A plugin merged into the community repo reaches magpie before npm has
// it: its first npm version waits for an owner's 2FA, which kept reviewed
// plugins unmerged (magpie-community/plugins#84, Open Design). The market
// shows its version and README, and adding it installs the dist branch's
// tarball with real bun.
func TestDistInstallsAPackageNPMDoesntHave(t *testing.T) {
	sandbox(t)
	const pkg = "@magpie-community/opencode-opendesign-auth"
	npmHasNothing(t)
	fakeDist(t, map[string][]string{pkg: {"0.1.3", "0.1.4"}}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if info := Info(ctx, []string{pkg})[pkg]; info.Version != "0.1.4" || info.Description != "from dist" || info.Publisher != "dbydd" {
		t.Fatalf("the market says %+v, want the dist branch's 0.1.4", info)
	}
	if p, err := Readme(ctx, pkg); err != nil || !strings.Contains(p.Readme, "from dist") {
		t.Fatalf("README: %+v, %v", p, err)
	}
	if _, err := Add(ctx, pkg); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if v := Installed(pkg); v != "0.1.4" {
		t.Fatalf("installed %q, want 0.1.4", v)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "dist", "magpie-community-opencode-opendesign-auth-0.1.4.tgz")); err != nil {
		t.Fatalf("the tarball bun.lock names isn't kept: %v", err)
	}
	// a pinned, older version too, and the newer one's tarball goes
	if err := installAt(ctx, pkg, "0.1.3"); err != nil {
		t.Fatalf("installAt 0.1.3: %v", err)
	}
	if v := Installed(pkg); v != "0.1.3" {
		t.Fatalf("installed %q, want 0.1.3", v)
	}
	if left, _ := filepath.Glob(filepath.Join(Dir(), "dist", "*.tgz")); len(left) != 1 || !strings.HasSuffix(left[0], "-0.1.3.tgz") {
		t.Fatalf("plugins/dist holds %v, want 0.1.3's tarball alone", left)
	}
}

// A version npm refused (its Trusted Publisher allowing stage publish
// only, as zed 0.1.7 and zen-free 0.1.7 were) comes from the dist branch,
// and once npm has a newer one, from npm again, the tarball taken out.
func TestDistServesAVersionNPMRefused(t *testing.T) {
	sandbox(t)
	const pkg = "@magpie-community/opencode-zed-auth"
	old := time.Now().Add(-72 * time.Hour)
	heldRegistry(t, pkg, map[string]time.Time{"0.1.6": old}, "0.1.6")
	fakeDist(t, map[string][]string{pkg: {"0.1.6", "0.1.7"}}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if _, err := Add(ctx, pkg+"@0.1.6"); err != nil {
		t.Fatal(err)
	}
	if err := save(List{Plugins: []Entry{{Spec: pkg + "@latest"}}}); err != nil {
		t.Fatal(err)
	}
	if err := Upgrade(ctx, pkg); err != nil {
		t.Fatalf("Upgrade to the dist branch's 0.1.7: %v", err)
	}
	if v := Installed(pkg); v != "0.1.7" {
		t.Fatalf("installed %q, want 0.1.7", v)
	}

	heldRegistry(t, pkg, map[string]time.Time{"0.1.6": old, "0.1.8": old}, "0.1.8")
	fakeDist(t, map[string][]string{pkg: {"0.1.6", "0.1.7", "0.1.8"}}, false)
	if err := Upgrade(ctx, pkg); err != nil {
		t.Fatalf("Upgrade to npm's 0.1.8: %v", err)
	}
	if v := Installed(pkg); v != "0.1.8" {
		t.Fatalf("installed %q, want 0.1.8", v)
	}
	b, _ := os.ReadFile(filepath.Join(Dir(), "package.json"))
	if !strings.Contains(string(b), `"`+pkg+`": "0.1.8"`) {
		t.Fatalf("plugins/package.json doesn't name npm's 0.1.8:\n%s", b)
	}
	if left, _ := filepath.Glob(filepath.Join(Dir(), "dist", "*.tgz")); len(left) != 0 {
		t.Fatalf("plugins/dist still holds %v", left)
	}
}

// A tarball that isn't the one the index names is never installed.
func TestDistRefusesATarballNotItsIndexs(t *testing.T) {
	sandbox(t)
	const pkg = "@magpie-community/opencode-opendesign-auth"
	npmHasNothing(t)
	fakeDist(t, map[string][]string{pkg: {"0.1.4"}}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, err := Add(ctx, pkg)
	if err == nil || !strings.Contains(err.Error(), "isn't the tarball its index names") {
		t.Fatalf("Add: %v, want the sha512 refused", err)
	}
	if v := Installed(pkg); v != "" {
		t.Fatalf("installed %q", v)
	}
	if left, _ := filepath.Glob(filepath.Join(Dir(), "dist", "*")); len(left) != 0 {
		t.Fatalf("plugins/dist holds %v", left)
	}
}

// pruneDist keeps a tarball plugins/package.json names or bun.lock's
// packages has a package at, read as Bun writes them. gkd2323c's lock
// (#3622) had three packages at the tarballs before the ones package.json
// names, and c158b043 took those out; a package.json Bun 1.4.0 wrote names
// autoclaw twice, the new tarball first, which Bun reads, and c158b043 kept
// only the second. The tarball Bun 1.4.2 leaves in the lock's workspaces
// alone goes, as does one nothing names; with no bun.lock, package.json
// alone decides; with one that can't be read, nothing goes.
func TestDistTarballTheLockNamesStays(t *testing.T) {
	const (
		autoclaw    = "magpie-community-opencode-autoclaw-auth-0.2.6.tgz"
		autoclawWas = "magpie-community-opencode-autoclaw-auth-0.2.1.tgz"
		cloudflare  = "magpie-community-opencode-cloudflare-workers-ai-auth-0.0.3.tgz"
		cfWas       = "magpie-community-opencode-cloudflare-workers-ai-auth-0.0.2.tgz"
		codearts    = "magpie-community-opencode-codearts-auth-0.2.1.tgz"
		codeartsWas = "magpie-community-opencode-codearts-auth-0.2.0.tgz"
		unnamed     = "magpie-community-opencode-autoclaw-auth-0.2.5.tgz"
	)
	// gkd2323c's package.json and lock: the lines #3622 quotes, in the
	// files Bun writes, the lock's workspaces as package.json (the issue
	// says they were)
	reported := `{
  "name": "magpie-plugins",
  "private": true,
  "dependencies": {
    "@magpie-community/opencode-autoclaw-auth": "./dist/magpie-community-opencode-autoclaw-auth-0.2.6.tgz",
    "@magpie-community/opencode-cloudflare-workers-ai-auth": "./dist/magpie-community-opencode-cloudflare-workers-ai-auth-0.0.3.tgz",
    "@magpie-community/opencode-codearts-auth": "./dist/magpie-community-opencode-codearts-auth-0.2.1.tgz"
  }
}
`
	reportedLock := `{
  "lockfileVersion": 2,
  "configVersion": 1,
  "workspaces": {
    "": {
      "name": "magpie-plugins",
      "dependencies": {
        "@magpie-community/opencode-autoclaw-auth": "./dist/magpie-community-opencode-autoclaw-auth-0.2.6.tgz",
        "@magpie-community/opencode-cloudflare-workers-ai-auth": "./dist/magpie-community-opencode-cloudflare-workers-ai-auth-0.0.3.tgz",
        "@magpie-community/opencode-codearts-auth": "./dist/magpie-community-opencode-codearts-auth-0.2.1.tgz",
      },
    },
  },
  "packages": {
    "@magpie-community/opencode-autoclaw-auth": ["@magpie-community/opencode-autoclaw-auth@./dist/magpie-community-opencode-autoclaw-auth-0.2.1.tgz", {}, "sha512-..."],

    "@magpie-community/opencode-cloudflare-workers-ai-auth": ["@magpie-community/opencode-cloudflare-workers-ai-auth@./dist/magpie-community-opencode-cloudflare-workers-ai-auth-0.0.2.tgz", {}, "sha512-..."],

    "@magpie-community/opencode-codearts-auth": ["@magpie-community/opencode-codearts-auth@./dist/magpie-community-opencode-codearts-auth-0.2.0.tgz", {}, "sha512-..."],
  }
}
`
	// `bun add ./dist/<0.2.6>` over 0.2.1: Bun 1.4.2 writes package.json
	// so, Bun 1.4.0 with the package twice, and both write the lock below
	once := `{
  "name": "magpie-plugins",
  "private": true,
  "dependencies": {
    "@magpie-community/opencode-autoclaw-auth": "./dist/magpie-community-opencode-autoclaw-auth-0.2.6.tgz"
  }
}
`
	twice := `{
  "name": "magpie-plugins",
  "private": true,
  "dependencies": {
    "@magpie-community/opencode-autoclaw-auth": "./dist/magpie-community-opencode-autoclaw-auth-0.2.6.tgz",
    "@magpie-community/opencode-autoclaw-auth": "./dist/magpie-community-opencode-autoclaw-auth-0.2.1.tgz"
  }
}
`
	updatedLock := `{
  "lockfileVersion": 2,
  "configVersion": 1,
  "workspaces": {
    "": {
      "name": "magpie-plugins",
      "dependencies": {
        "@magpie-community/opencode-autoclaw-auth": "./dist/magpie-community-opencode-autoclaw-auth-0.2.6.tgz",
        "@magpie-community/opencode-autoclaw-auth": "./dist/magpie-community-opencode-autoclaw-auth-0.2.1.tgz",
      },
    },
  },
  "packages": {
    "@magpie-community/opencode-autoclaw-auth": ["@magpie-community/opencode-autoclaw-auth@./dist/magpie-community-opencode-autoclaw-auth-0.2.6.tgz", {}, "sha512-63rE5gosmPjiHJlJZU3SImX8OBOk8pAKQHX5OAa5Ke2vaSlhVgDzmyPiiiXOJ2fCHMs8ty5cYoMhcOzeaDLhvA=="],
  }
}
`
	for _, c := range []struct {
		name, pj, lock string // lock: "" for none, "unreadable" for a folder in its place
		stay, gone     []string
	}{
		{"the lock at the tarballs before", reported, reportedLock, []string{autoclaw, autoclawWas, cloudflare, cfWas, codearts, codeartsWas}, []string{unnamed}},
		{"package.json naming a package twice", twice, updatedLock, []string{autoclaw, autoclawWas}, []string{unnamed}},
		{"package.json naming a package twice, no lock", twice, "", []string{autoclaw, autoclawWas}, []string{unnamed}},
		{"the lock's workspaces alone naming one", once, updatedLock, []string{autoclaw}, []string{autoclawWas, unnamed}},
		{"no lock", reported, "", []string{autoclaw, cloudflare, codearts}, []string{autoclawWas, cfWas, codeartsWas, unnamed}},
		{"a lock that can't be read", reported, "unreadable", []string{autoclaw, autoclawWas, cloudflare, cfWas, codearts, codeartsWas, unnamed}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("USERPROFILE", dir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			if err := os.MkdirAll(filepath.Join(Dir(), "dist"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(Dir(), "package.json"), []byte(c.pj), 0o644); err != nil {
				t.Fatal(err)
			}
			switch c.lock {
			case "":
			case "unreadable":
				if err := os.Mkdir(filepath.Join(Dir(), "bun.lock"), 0o755); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(filepath.Join(Dir(), "bun.lock"), []byte(c.lock), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range append(slices.Clone(c.stay), c.gone...) {
				if err := os.WriteFile(filepath.Join(Dir(), "dist", f), []byte("tgz"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			pruneDist()
			for _, f := range c.stay {
				if _, err := os.Stat(filepath.Join(Dir(), "dist", f)); err != nil {
					t.Errorf("%s taken out: %v", f, err)
				}
			}
			for _, f := range c.gone {
				if _, err := os.Stat(filepath.Join(Dir(), "dist", f)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s kept (%v)", f, err)
				}
			}
		})
	}
}

func TestDistWanted(t *testing.T) {
	for spec, want := range map[string]string{
		"@magpie-community/opencode-zed-auth":           "@magpie-community/opencode-zed-auth latest",
		"@magpie-community/opencode-zed-auth@latest":    "@magpie-community/opencode-zed-auth latest",
		"@magpie-community/opencode-zed-auth@0.1.7":     "@magpie-community/opencode-zed-auth 0.1.7",
		"@magpie-community/opencode-zed-auth@^0.1.7":    "",
		"@magpie-community/opencode-zed-auth@next":      "",
		"opencode-zed-auth@0.1.7":                       "",
		"@someone/opencode-zed-auth@0.1.7":              "",
		"github:magpie-community/plugins":               "",
		"./dist/magpie-community-opencode-zed-auth.tgz": "",
	} {
		name, version, ok := distWanted(spec)
		got := ""
		if ok {
			got = name + " " + version
		}
		if got != want {
			t.Errorf("distWanted(%q) = %q, want %q", spec, got, want)
		}
	}
	for file, ok := range map[string]bool{"tgz/a-0.1.0.tgz": true, "tgz/../a.tgz": false, "../a.tgz": false, "tgz/a/b.tgz": false, "tgz/a.zip": false} {
		if distFile.MatchString(file) != ok {
			t.Errorf("distFile %q: %v, want %v", file, !ok, ok)
		}
	}
}
