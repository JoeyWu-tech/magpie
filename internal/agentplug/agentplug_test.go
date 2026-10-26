package agentplug

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A module that never finishes loading, or whose connect never returns, is
// stopped at its limit, so neither the agents' list nor a wiring waits on
// it, and not before a second. This holds the limits at a second from
// below and a minute from above; the tests elsewhere take LimitsForTest's.
func TestModuleStoppedAtItsLimit(t *testing.T) {
	dir := t.TempDir()
	module := func(name, src string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// how long f took and what it returned, or the test's end if a minute
	// goes by first (f's goroutine then spins on in the module's loop until
	// the binary exits)
	stopped := func(what string, f func() error) (time.Duration, error) {
		t.Helper()
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- f() }()
		select {
		case err := <-done:
			return time.Since(start), err
		case <-time.After(time.Minute):
			t.Fatalf("%s wasn't stopped in a minute", what)
			return 0, nil
		}
	}
	// a timer never fires early, so a stop sooner than a second is a limit
	// shorter than one
	early := func(what string, d time.Duration) {
		t.Helper()
		if d < time.Second {
			t.Fatalf("%s was stopped after %v, before its second", what, d)
		}
	}

	file := module("load.js", "for (;;) {}\n")
	d, err := stopped("a module that loops as it loads", func() error {
		_, err := Compile(dir, file)
		return err
	})
	if !errors.Is(err, errTimeout) {
		t.Fatalf("a module that loops as it loads: %v, want %v", err, errTimeout)
	}
	early("a module that loops as it loads", d)

	file = module("connect.js", `export const agent = { id: "loop", config: "~/.loop.json", model: "model" };
export function connect() { for (;;) {} }
`)
	back := LimitsForTest() // loaded however long it takes: this half is about connect
	a, err := Compile(dir, file)
	back()
	if err != nil {
		t.Fatal(err)
	}
	d, err = stopped("a connect that loops", func() error {
		_, err := a.Connect(map[string]any{})
		return err
	})
	if err == nil || !strings.HasSuffix(err.Error(), errTimeout.Error()) {
		t.Fatalf("a connect that loops: %v, want %v", err, errTimeout)
	}
	early("a connect that loops", d)
}
