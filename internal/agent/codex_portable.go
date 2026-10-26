package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/sessions"
)

// A Codex conversation magpie compacted can't go on once Codex talks to
// OpenAI itself, unless its rollout is written portable
// (sessions.PortCodexRollouts, LukeMa on X): done while magpie runs, for
// the threads no Codex has open, and again as magpie lets Codex go (quit,
// Disconnect), for those it has just let go.

// codexPortEvery is how often a running magpie makes the rollouts written
// since portable; codexPortAfter how long after it starts it first does.
var codexPortEvery, codexPortAfter = 10 * time.Minute, 2 * time.Minute

// codexPortUnwire is how long Disconnect spends on it; what is left is
// done by the next pass.
const codexPortUnwire = 3 * time.Second

// portCodexHome makes the rollouts of the Codex home dir portable, for
// up to d.
func portCodexHome(dir string, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	res, err := sessions.PortCodexRollouts(ctx, dir)
	if err != nil {
		log.Println("codex rollouts:", err)
	}
	if len(res.Rewritten) > 0 || len(res.Held) > 0 {
		log.Printf("codex rollouts: %d written portable, %d left while a Codex has them open", len(res.Rewritten), len(res.Held))
	}
	// by name: a thread whose latest compaction is one of these goes on
	// only through magpie, which matters most as magpie lets Codex go
	if len(res.Left) > 0 {
		names := make([]string, len(res.Left))
		for i, p := range res.Left {
			names[i] = filepath.Base(p)
		}
		log.Printf("codex rollouts: %d keep a compaction of magpie's whose message is longer than it, so a history up to it goes on only through magpie: %s", len(res.Left), strings.Join(names, ", "))
	}
}

// PortCodexRollouts makes this machine's Codex rollouts portable, for up
// to d: as magpie quits.
func PortCodexRollouts(d time.Duration) {
	if home, err := os.UserHomeDir(); err == nil {
		portCodexHome(here(home).codexHome(), d)
	}
}

// KeepCodexRolloutsPortable makes this machine's Codex rollouts portable
// every so often until ctx ends.
func KeepCodexRolloutsPortable(ctx context.Context) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := here(home).codexHome()
	wait := codexPortAfter
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = codexPortEvery
		// a pass that is cut short goes on from where it was next time
		portCodexHome(dir, codexPortEvery/2)
	}
}
