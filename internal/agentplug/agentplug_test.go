package agentplug

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// A file an agent module imports is looked at as the module's own file is
// (#3723): one edited, and one that made the module fail and is fixed.
func TestReloadOnImportChange(t *testing.T) {
	dir := t.TempDir()
	write := func(path, src string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "package.json"), `{"name":"pa","magpie":{"agent":"./agent.js"}}`)
	write(filepath.Join(dir, "agent.js"), `import { id, name } from "./id.js";
export const agent = { id, name, config: "~/.probe.json", model: "model" };`)
	write(filepath.Join(dir, "id.js"), `export const id = "probe", name = "A";`)
	if err := os.MkdirAll(settings.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(plugin.List{Plugins: []plugin.Entry{{Spec: dir}}})
	write(filepath.Join(settings.Dir(), "plugins.json"), string(b))
	Reload()
	t.Cleanup(func() {
		os.Remove(filepath.Join(settings.Dir(), "plugins.json"))
		Reload()
	})
	// the agent's name, and why the module didn't load
	list := func() (string, string) {
		t.Helper()
		nextCheck.Store(0)
		l, errs := List()
		name := ""
		if len(l) == 1 {
			name = l[0].Desc.Name
		}
		return name, errs[dir]
	}
	if n, err := list(); n != "A" || err != "" {
		t.Fatalf("first: %q, %q", n, err)
	}
	write(filepath.Join(dir, "id.js"), `export const id = "probe", name = "Bee";`)
	if n, err := list(); n != "Bee" || err != "" {
		t.Fatalf("after id.js was edited: %q, %q", n, err)
	}
	// compiled, and an id the agent export can't have
	write(filepath.Join(dir, "id.js"), `export const id = "Not An Id", name = "Bee";`)
	if n, err := list(); n != "" || !strings.Contains(err, "agent.id") {
		t.Fatalf("after id.js gave a bad id: %q, %q", n, err)
	}
	write(filepath.Join(dir, "id.js"), `export const id = "probe", name = "Cee";`)
	if n, err := list(); n != "Cee" || err != "" {
		t.Fatalf("after id.js was mended: %q, %q", n, err)
	}
}
