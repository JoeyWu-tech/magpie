package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/yetone/magpie/internal/codexsummary"
	"github.com/yetone/magpie/internal/filelock"
)

const portThread = "01a15820-ff81-7840-bb3f-e18771c53aa8"

// portRolloutLines is a Codex 0.162 rollout of a thread magpie compacted,
// in the shapes Codex writes (codex-rs/history/src/rollout_payload.rs,
// CompactedItemWire; the compaction item as magpie's gateway streams it):
// a session_meta, a user message that talks about "magpie1:" (text, never
// changed), magpie's compaction item as a response_item, the compacted
// checkpoint holding it in replacement_history with its metadata beside,
// a checkpoint with an unreadable one of magpie's, OpenAI's own sealed
// compaction, and a later turn.
func portRolloutLines(summary string) []string {
	enc := codexsummary.Encrypted(summary)
	return []string{
		`{"timestamp":"2026-10-20T07:05:20.118Z","type":"session_meta","payload":{"id":"` + portThread + `","timestamp":"2026-10-20T07:05:20.101Z","cwd":"/Users/me/w","originator":"codex_cli_rs","cli_version":"0.162.1","source":"cli","model_provider":"openai"}}`,
		`{"timestamp":"2026-10-20T07:05:31.002Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"why does \"magpie1:\" show in my rollout? <b>&</b>"}]}}`,
		`{"timestamp":"2026-10-20T07:40:02.551Z","type":"response_item","payload":{"type":"compaction","id":"cmp_magpie_1761000002","encrypted_content":"` + enc + `"},"metadata":{"compaction_model_hash":"h1"}}`,
		`{"timestamp":"2026-10-20T07:40:02.553Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]},{"type":"compaction","id":"cmp_magpie_1761000002","encrypted_content":"` + enc + `"}],"replacement_history_metadata":[{"client_authored":true},{},{"compaction_model_hash":"h1"}],"window_number":2,"compaction_response_id":"resp_magpie_1761000002","latest_token_usage_record":{"input_tokens":182734,"output_tokens":1204}}}`,
		`{"timestamp":"2026-10-20T08:10:00.000Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]},{"type":"compaction","encrypted_content":"magpie1:%%%"}],"replacement_history_metadata":[{"client_authored":true},{"compaction_model_hash":"h2"}]}}`,
		`{"timestamp":"2026-10-20T09:00:00.000Z","type":"response_item","payload":{"type":"compaction","id":"cmp_0a1b","encrypted_content":"gAAAAABo1x2y3z_OpenAIsOwn=="}}`,
		`{"timestamp":"2026-10-20T09:00:05.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done: 1.5e+300 tokens, <ok>"}]}}`,
	}
}

// portHome is a Codex home with thread-writer-locks, as Codex 0.162 keeps
// it, and the rollout at sessions/2026/10/20, plain or packed, last
// written an hour ago with mode 0640.
func portHome(t *testing.T, lines []string, zst bool) (home, path string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	home = filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(filepath.Join(home, "thread-writer-locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions", "2026", "10", "20")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "rollout-2026-10-20T15-05-20-"+portThread+".jsonl")
	b := []byte(strings.Join(lines, "\n") + "\n")
	if zst {
		path += ".zst"
		z, _ := zstd.NewWriter(nil)
		b = z.EncodeAll(b, nil)
	}
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func readRollout(t *testing.T, path string) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(string(rolloutBytes(t, path)), "\n"), "\n")
}

// rolloutBytes is the rollout at path as Codex reads it, a packed one
// decompressed: what its offsets count.
func rolloutBytes(t *testing.T, path string) []byte {
	t.Helper()
	in, err := openLines(path)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var buf bytes.Buffer
	buf.ReadFrom(in)
	return buf.Bytes()
}

// lineStarts is where each line of b starts.
func lineStarts(b []byte) []int {
	at := []int{0}
	for i, c := range b {
		if c == '\n' && i+1 < len(b) {
			at = append(at, i+1)
		}
	}
	return at
}

// longSummary is a summary as long as a model's usually is, well past the
// 1.1 to 1.3KB under which Codex's message for it comes out longer than
// magpie's item.
func longSummary() string {
	var b strings.Builder
	b.WriteString("## Progress\n")
	for i := 1; b.Len() < 4000; i++ {
		fmt.Fprintf(&b, "- Step %d: ran `go test ./internal/x/...`, then kept the request's \"trace id\" across retries in handler_%d.go.\n", i, i)
	}
	b.WriteString("## Next\n- Run the suite.\n")
	return b.String()
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	return v
}

// LukeMa (X): a Codex conversation magpie compacted errored once magpie
// was quit, as OpenAI can't read magpie's compaction item
// (invalid_encrypted_content). PortCodexRollouts writes each of magpie's
// items as Codex's own summary message, the one the gateway sends for it,
// and changes nothing else: the other lines keep their bytes, the file
// its line count, mode and time, and a compacted line's metadata stays
// beside its items. Every line starts where it did, as Codex reads the
// file at offsets it keeps (a fork's or a rollback's history_base,
// ianchenx in #3616).
func TestPortCodexRollouts(t *testing.T) {
	for _, zst := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "zst"}[zst], func(t *testing.T) {
			summary := longSummary()
			lines := portRolloutLines(summary)
			home, path := portHome(t, lines, zst)
			before, _ := os.Stat(path)
			was := []byte(strings.Join(lines, "\n") + "\n")

			res, err := PortCodexRollouts(context.Background(), home)
			if err != nil || !res.Done || len(res.Rewritten) != 1 || len(res.Held) != 0 || len(res.Left) != 0 {
				t.Fatalf("pass: %+v, %v", res, err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
				t.Errorf("time/mode %v %v, was %v %v", after.ModTime(), after.Mode(), before.ModTime(), before.Mode())
			}
			now := rolloutBytes(t, path)
			if len(now) != len(was) {
				t.Errorf("%d bytes, was %d", len(now), len(was))
			}
			for i, at := range lineStarts(was) {
				if s := lineStarts(now); i >= len(s) || s[i] != at {
					t.Errorf("line %d doesn't start at %d, where Codex reads it", i+1, at)
					break
				}
			}
			got := readRollout(t, path)
			if len(got) != len(lines) {
				t.Fatalf("%d lines, was %d", len(got), len(lines))
			}
			for _, i := range []int{0, 1, 5, 6} {
				if got[i] != lines[i] {
					t.Errorf("line %d changed:\n%s\nwas\n%s", i, got[i], lines[i])
				}
			}
			want := codexsummary.UserMessage(codexsummary.Prefix + "\n" + summary)
			wantJSON, _ := json.Marshal(want)
			var wantAny any
			json.Unmarshal(wantJSON, &wantAny)

			l2 := decode(t, got[2])
			if !reflect.DeepEqual(l2["payload"], wantAny) {
				t.Errorf("response_item payload %v, want %v", l2["payload"], wantAny)
			}
			if !reflect.DeepEqual(l2["metadata"], map[string]any{"compaction_model_hash": "h1"}) || l2["type"] != "response_item" {
				t.Errorf("the rest of the line changed: %s", got[2])
			}
			p3 := decode(t, got[3])["payload"].(map[string]any)
			hist := p3["replacement_history"].([]any)
			if len(hist) != 3 || !reflect.DeepEqual(hist[2], wantAny) {
				t.Errorf("replacement_history %v", hist)
			}
			if meta := p3["replacement_history_metadata"].([]any); len(meta) != 3 {
				t.Errorf("metadata %v", meta)
			}
			if p3["window_number"] != float64(2) || p3["compaction_response_id"] != "resp_magpie_1761000002" {
				t.Errorf("checkpoint fields changed: %s", got[3])
			}
			// the unreadable one is left out with its metadata entry, as the
			// gateway leaves it out of what it sends
			p4 := decode(t, got[4])["payload"].(map[string]any)
			if h := p4["replacement_history"].([]any); len(h) != 1 {
				t.Errorf("replacement_history %v", h)
			}
			if m := p4["replacement_history_metadata"].([]any); len(m) != 1 || !reflect.DeepEqual(m[0], map[string]any{"client_authored": true}) {
				t.Errorf("metadata %v", m)
			}
			// nothing of magpie's that OpenAI would refuse is left
			for i, l := range got {
				if i != 1 && strings.Contains(l, codexsummary.Mark) {
					t.Errorf("line %d still has magpie's item: %s", i, l)
				}
			}
			// a second pass has nothing to do
			if res, err := PortCodexRollouts(context.Background(), home); err != nil || len(res.Rewritten) != 0 {
				t.Errorf("second pass: %+v, %v", res, err)
			}
		})
	}
}

// A rollout whose thread a Codex has open (its writer lock held) is never
// written under it: it is left as it was, kept for a later pass, and
// written once the Codex lets it go. A Codex home without
// thread-writer-locks, where there is no telling, is left alone.
func TestPortCodexRolloutsLeavesAnOpenThread(t *testing.T) {
	lines := portRolloutLines(longSummary())
	home, path := portHome(t, lines, false)
	orig, _ := os.ReadFile(path)
	lf, err := os.OpenFile(filepath.Join(home, "thread-writer-locks", portThread+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if got, err := filelock.TryLock(lf); !got || err != nil {
		t.Fatalf("lock: %v %v", got, err)
	}
	res, err := PortCodexRollouts(context.Background(), home)
	if err != nil || len(res.Held) != 1 || len(res.Rewritten) != 0 {
		t.Fatalf("pass with the thread open: %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, orig) {
		t.Fatal("written while a Codex had the thread open")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".magpie-*")); len(left) > 0 {
		t.Fatalf("left behind: %v", left)
	}
	filelock.Unlock(lf)
	// the pass after looks again at what it left, though its time is old
	res, err = PortCodexRollouts(context.Background(), home)
	if err != nil || len(res.Rewritten) != 1 {
		t.Fatalf("pass after: %+v, %v", res, err)
	}

	home2, path2 := portHome(t, lines, false)
	os.Remove(filepath.Join(home2, "thread-writer-locks"))
	orig2, _ := os.ReadFile(path2)
	if res, err := PortCodexRollouts(context.Background(), home2); err != nil || len(res.Rewritten) != 0 {
		t.Fatalf("no writer locks: %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(path2); !bytes.Equal(b, orig2) {
		t.Fatal("written without Codex's writer locks")
	}
}

// A line is parsed only for magpie's item; text that only mentions
// "magpie1:" keeps its bytes, and a line that is one unreadable item of
// magpie's alone stays (leaving it out would move the lines after it).
func TestPortLineLeavesOthers(t *testing.T) {
	for _, l := range []string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"magpie1:abc"}]}}` + "\n",
		`{"type":"response_item","payload":{"type":"compaction","encrypted_content":"magpie1:%%%"}}` + "\r\n",
		`{"type":"response_item","payload":{"type":"reasoning","encrypted_content":"magpie1:xyz"}}` + "\n",
		`not json magpie1:` + "\n",
	} {
		if got, ok, long := portLine([]byte(l)); ok || long || string(got) != l {
			t.Errorf("%q became %q", l, got)
		}
	}
	l := `{"type":"response_item","payload":{"type":"compaction_summary","encrypted_content":"` + codexsummary.Encrypted(longSummary()) + `"}}` + "\r\n"
	got, ok, long := portLine([]byte(l))
	if !ok || long || !bytes.HasSuffix(bytes.TrimRight(got, " \r\n"), []byte("}")) || !bytes.HasSuffix(got, []byte("\r\n")) || bytes.Contains(got, []byte("magpie1:")) {
		t.Errorf("compaction_summary: %q", got)
	}
}

// A line written as its message keeps its length and its line end ("\n",
// "\r\n", or none on a last line), padded with spaces a JSON reader skips;
// one whose message would be longer than magpie's item is given as it is.
func TestPortLineKeepsItsLength(t *testing.T) {
	head := `{"timestamp":"2026-10-20T07:40:02.551Z","type":"response_item","payload":{"type":"compaction","id":"cmp_magpie_1761000002","encrypted_content":"`
	for _, end := range []string{"\n", "\r\n", ""} {
		l := head + codexsummary.Encrypted(longSummary()) + `"}}` + end
		got, ok, long := portLine([]byte(l))
		if !ok || long {
			t.Fatalf("not written (long %v): %q", long, l[:80])
		}
		if len(got) != len(l) || !strings.HasSuffix(string(got), end) {
			t.Errorf("%d bytes ending %q, was %d ending %q", len(got), got[len(got)-len(end):], len(l), end)
		}
		body := bytes.TrimRight(got, "\r\n")
		if !json.Valid(body) || bytes.TrimRight(body, " ")[len(bytes.TrimRight(body, " "))-1] != '}' {
			t.Errorf("not one JSON value then spaces: %q", body[len(body)-40:])
		}
	}
	short := head + codexsummary.Encrypted("Fixed the build. Next: the suite.") + `"}}` + "\n"
	if got, ok, long := portLine([]byte(short)); ok || !long || string(got) != short {
		t.Errorf("a message longer than the item: changed %v, long %v, %q", ok, long, got)
	}
}

// A message longer than magpie's item, as a summary under about 1.1 to
// 1.3KB gives, can't take its place without moving every line after it,
// which Codex reads at offsets it keeps (#3616). Such a line is kept as it
// is, and the rollout's other items are written where they fit: Codex
// sends only its latest compaction's item, so a thread compacted again
// with a longer summary goes on without magpie. A rollout with a line kept
// is counted as left, not as written; one with no item that fits is left
// byte for byte.
func TestPortCodexRolloutsKeepsALineThatDoesntFit(t *testing.T) {
	short := "Fixed the build (pool.go). Next: the suite."
	first, later := portRolloutLines(short), portRolloutLines(longSummary())
	for i := range later {
		later[i] = strings.ReplaceAll(later[i], "1761000002", "1761003600")
	}
	// the short summary's item and checkpoint first, the long one's last
	lines := []string{first[0], first[1], first[2], first[3], first[6], later[2], later[3], later[6]}
	home, path := portHome(t, lines, false)
	was := []byte(strings.Join(lines, "\n") + "\n")
	res, err := PortCodexRollouts(context.Background(), home)
	if err != nil || !res.Done || len(res.Rewritten) != 0 || len(res.Left) != 1 || len(res.Held) != 0 {
		t.Errorf("pass: %+v, %v", res, err)
	}
	now := rolloutBytes(t, path)
	if len(now) != len(was) {
		t.Errorf("%d bytes, was %d", len(now), len(was))
	}
	for i, at := range lineStarts(was) {
		if s := lineStarts(now); i >= len(s) || s[i] != at {
			t.Errorf("line %d doesn't start at %d, where Codex reads it", i+1, at)
			break
		}
	}
	got := readRollout(t, path)
	if len(got) != len(lines) {
		t.Fatalf("%d lines, was %d", len(got), len(lines))
	}
	for _, i := range []int{0, 1, 2, 3, 4, 7} {
		if got[i] != lines[i] {
			t.Errorf("line %d changed:\n%s\nwas\n%s", i, got[i], lines[i])
		}
	}
	want := codexsummary.UserMessage(codexsummary.Prefix + "\n" + longSummary())
	wantJSON, _ := json.Marshal(want)
	var wantAny any
	json.Unmarshal(wantJSON, &wantAny)
	if p := decode(t, got[5])["payload"]; !reflect.DeepEqual(p, wantAny) {
		t.Errorf("the later item: %v", p)
	}
	if h := decode(t, got[6])["payload"].(map[string]any)["replacement_history"].([]any); len(h) != 3 || !reflect.DeepEqual(h[2], wantAny) {
		t.Errorf("the later checkpoint: %v", h)
	}
	// a second pass has nothing to do
	if res, err := PortCodexRollouts(context.Background(), home); err != nil || len(res.Rewritten) != 0 || len(res.Left) != 0 {
		t.Errorf("second pass: %+v, %v", res, err)
	}

	// every item short (and none unreadable, which would go)
	home, path = portHome(t, []string{first[0], first[1], first[2], first[3], first[5], first[6]}, false)
	orig, _ := os.ReadFile(path)
	res, err = PortCodexRollouts(context.Background(), home)
	if err != nil || len(res.Left) != 1 || len(res.Rewritten) != 0 || len(res.Held) != 0 {
		t.Fatalf("pass, every item short: %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, orig) {
		t.Fatal("written though no item of magpie's fits")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".magpie-*")); len(left) > 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// A message exactly as long as magpie's item takes its place with nothing
// padded, and one a byte longer is kept as it is: the item's id is in the
// item and not in the message, so each character of it moves the item's
// length against the message's by one.
func TestPortLineAtTheEdge(t *testing.T) {
	summary := strings.Repeat("Fixed the build in pool.go, then ran the suite. ", 21)
	line := func(id int, end string) string {
		return `{"timestamp":"2026-10-20T07:40:02.551Z","type":"response_item","payload":{"type":"compaction","id":"cmp_` + strings.Repeat("7", id) + `","encrypted_content":"` + codexsummary.Encrypted(summary) + `"}}` + end
	}
	for _, end := range []string{"\n", "\r\n"} {
		fit := -1
		for id := 0; id < 300 && fit < 0; id++ {
			if _, ok, _ := portLine([]byte(line(id, end))); ok {
				fit = id
			}
		}
		if fit < 1 {
			t.Fatalf("no id from which the message fits, or it fits from the shortest (%d)", fit)
		}
		l := line(fit, end)
		got, ok, long := portLine([]byte(l))
		body := bytes.TrimSuffix(got, []byte(end))
		if !ok || long || len(got) != len(l) || bytes.HasSuffix(body, []byte(" ")) {
			t.Errorf("exactly the item's length: changed %v, long %v, %d bytes for %d, %q", ok, long, len(got), len(l), got[len(got)-len(end)-8:])
		}
		l = line(fit+1, end)
		if got, ok, _ := portLine([]byte(l)); !ok || len(got) != len(l) || !bytes.HasSuffix(got, []byte("} "+end)) {
			t.Errorf("a byte to spare: changed %v, %d bytes for %d, %q", ok, len(got), len(l), got[len(got)-len(end)-8:])
		}
		l = line(fit-1, end)
		if got, ok, long := portLine([]byte(l)); ok || !long || string(got) != l {
			t.Errorf("a byte longer than the item: changed %v, long %v, written %v", ok, long, string(got) != l)
		}
	}
}

// A pass writes nothing unless the rollout comes out the size it was: with
// a port that wrote a line short of its length, the rollout stays byte for
// byte, nothing is left beside it, and the reason names both sizes.
func TestPortRolloutWritesNothingOfAnotherSize(t *testing.T) {
	home, path := portHome(t, portRolloutLines(longSummary()), false)
	orig, _ := os.ReadFile(path)
	was := portedLine
	t.Cleanup(func() { portedLine = was })
	portedLine = func(line []byte) ([]byte, bool, bool) {
		out, ok, long := portLine(line)
		if ok {
			out = append(bytes.TrimRight(out, " \r\n"), '\n')
		}
		return out, ok, long
	}
	if res, err := PortCodexRollouts(context.Background(), home); err != nil || len(res.Rewritten) != 0 || len(res.Left) != 0 || len(res.Held) != 0 {
		t.Errorf("pass: %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, orig) {
		t.Fatal("written though a line came out shorter")
	}
	written, kept, err := portRollout(context.Background(), home, path)
	if written || kept || err == nil || !strings.Contains(err.Error(), fmt.Sprintf("written for its %d", len(orig))) {
		t.Errorf("written %v, kept %v, %v", written, kept, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".magpie-*")); len(left) > 0 {
		t.Fatalf("left behind: %v", left)
	}
}
