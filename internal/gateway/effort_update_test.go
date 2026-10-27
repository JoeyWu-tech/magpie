package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// effortTurns are a thread's requests as an agent sends them: the
// history so far, each turn adding the reply and the next user message.
func effortTurn(key, effort string, users ...string) string {
	var items []string
	for i, u := range users {
		if i > 0 {
			items = append(items, fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reply %d"}]}`, i))
		}
		items = append(items, fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}`, u))
	}
	return fmt.Sprintf(`{"model":"codex/gpt-6-luna","stream":true,"prompt_cache_key":%q,"reasoning":{"effort":%q},"input":[%s]}`, key, effort, strings.Join(items, ","))
}

// sentShape is what the upstream got: the top-level effort, the
// prompt_cache_key, and the input as roles and updates ("u:high").
func sentShape(t *testing.T, b []byte) (effort, key string, input []string) {
	t.Helper()
	var q struct {
		Key       string `json:"prompt_cache_key"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Input []struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		} `json:"input"`
	}
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatalf("upstream body: %v: %s", err, b)
	}
	for _, it := range q.Input {
		if it.Type == "configuration_update" {
			input = append(input, "u:"+it.Reasoning.Effort)
		} else {
			input = append(input, it.Role)
		}
	}
	return q.Reasoning.Effort, q.Key, input
}

func effortCodexAccount(t *testing.T, f *fake) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	claims := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
	}
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	auth := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":%q,"refresh_token":"r","account_id":"acct-1"}}`,
		claims(map[string]any{"email": "me@example.com"}), claims(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}))
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(auth), 0o600)
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	old := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = old })
	effortThreads.Lock()
	effortThreads.m, effortThreads.no = map[string]*effortThread{}, map[string]time.Time{}
	effortThreads.Unlock()
}

var effortReply = sse(
	`data: {"type":"response.created","response":{"id":"r1","model":"gpt-6-luna"}}`,
	`data: {"type":"response.output_text.delta","delta":"pong"}`,
	`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":7,"output_tokens":1}}}`)

// #617: an effort lowered mid-thread goes as a configuration_update before
// the user message it came with, the thread's effort staying at the top,
// and is put back in the same place on every later turn; one raised above
// it starts the thread again at the top, since the backend gives an update
// up a fraction of the reasoning asked; a thread that keeps its effort
// goes as the agent sent it.
func TestEffortChangeKeepsTheCache(t *testing.T) {
	t.Setenv("MAGPIE_EFFORT_UPDATES", "on")
	f := &fake{t: t, ctype: "none", reply: effortReply}
	effortCodexAccount(t, f)

	type want struct {
		effort string
		input  string
	}
	for i, tc := range []struct {
		body string
		want want
	}{
		{effortTurn("t1", "low", "a"), want{"low", "user"}},
		{effortTurn("t1", "low", "a", "b"), want{"low", "user,assistant,user"}},
		// up: the thread starts again at it, at the top — an update gets
		// a fraction of the reasoning asked
		{effortTurn("t1", "high", "a", "b", "c"), want{"high", "user,assistant,user,assistant,user"}},
		{effortTurn("t1", "high", "a", "b", "c", "d"), want{"high", "user,assistant,user,assistant,user,assistant,user"}},
		// down: an update before the user message it came with, the top
		// unchanged, put back in the same place on later turns
		{effortTurn("t1", "medium", "a", "b", "c", "d", "e"), want{"high", "user,assistant,user,assistant,user,assistant,user,assistant,u:medium,user"}},
		{effortTurn("t1", "low", "a", "b", "c", "d", "e", "f"), want{"high", "user,assistant,user,assistant,user,assistant,user,assistant,u:medium,user,assistant,u:low,user"}},
		// back to the first effort: an update to it, the top unchanged
		{effortTurn("t1", "high", "a", "b", "c", "d", "e", "f", "g"), want{"high", "user,assistant,user,assistant,user,assistant,user,assistant,u:medium,user,assistant,u:low,user,assistant,u:high,user"}},
		// another thread, at one effort throughout, is left alone
		{effortTurn("t2", "high", "x"), want{"high", "user"}},
		{effortTurn("t2", "high", "x", "y"), want{"high", "user,assistant,user"}},
		// a history that isn't the last one and more (compacted) starts
		// the thread again at the effort it asks
		{effortTurn("t1", "low", "summary", "h"), want{"low", "user,assistant,user"}},
	} {
		code, body := post(t, "/v1/responses", tc.body)
		if code != 200 || !strings.Contains(body, "pong") {
			t.Fatalf("turn %d: status %d: %s", i, code, body)
		}
		effort, key, input := sentShape(t, f.got)
		var thread struct {
			Key string `json:"prompt_cache_key"`
		}
		json.Unmarshal([]byte(tc.body), &thread)
		if effort != tc.want.effort || strings.Join(input, ",") != tc.want.input || key != thread.Key {
			t.Fatalf("turn %d: sent effort %q, key %q, input %s; want %q, %q, %s", i, effort, key, strings.Join(input, ","), tc.want.effort, thread.Key, tc.want.input)
		}
	}
}

// #617: an upstream that turns the update away is asked again as the agent
// sent it, and that account and model aren't sent updates again.
func TestEffortUpdateRefused(t *testing.T) {
	t.Setenv("MAGPIE_EFFORT_UPDATES", "on")
	f := &fake{t: t, ctype: "none", reply: effortReply, refuse: func(b []byte) (int, string) {
		if strings.Contains(string(b), "configuration_update") {
			return 400, `{"error":{"message":"Invalid value: 'configuration_update'.","type":"invalid_request_error","param":"input[2].type"}}`
		}
		return 0, ""
	}}
	effortCodexAccount(t, f)

	post(t, "/v1/responses", effortTurn("t1", "high", "a"))
	f.calls = 0
	code, body := post(t, "/v1/responses", effortTurn("t1", "low", "a", "b"))
	effort, _, input := sentShape(t, f.got)
	if code != 200 || f.calls != 2 || effort != "low" || strings.Join(input, ",") != "user,assistant,user" {
		t.Fatalf("refused: status %d after %d calls, sent %q %v: %s", code, f.calls, effort, input, body)
	}
	f.calls = 0
	code, _ = post(t, "/v1/responses", effortTurn("t1", "medium", "a", "b", "c"))
	effort, _, input = sentShape(t, f.got)
	if code != 200 || f.calls != 1 || effort != "medium" || strings.Contains(strings.Join(input, ","), "u:") {
		t.Fatalf("after: status %d after %d calls, sent %q %v", code, f.calls, effort, input)
	}
}

// #617: only a GPT-6 model through a ChatGPT account or OpenAI's API is
// sent updates; another vendor's Responses API, and older models, never.
func TestTakesEffortUpdates(t *testing.T) {
	t.Setenv("MAGPIE_EFFORT_UPDATES", "on")
	codex := provider.Provider{ID: "codex", Responses: provider.CodexBase, Account: &provider.Account{Agent: "codex"}}
	openai := provider.Provider{ID: "openai", Responses: "https://api.openai.com/v1", Key: "k"}
	other := provider.Provider{ID: "relay", Responses: "https://relay.example.com/v1", Key: "k"}
	xai := provider.Provider{ID: "xai", Responses: "https://api.x.ai/v1", Key: "k"}
	chatOnly := provider.Provider{ID: "openai-chat", Chat: "https://api.openai.com/v1", Key: "k"}
	for _, tc := range []struct {
		p     provider.Provider
		model string
		want  bool
	}{
		{codex, "gpt-6-luna", true},
		{codex, "gpt-6.1-sol", true},
		{openai, "gpt-6-astra", true},
		{openai, "openai/gpt-6-astra", true},
		{codex, "gpt-5.5", false},
		{other, "gpt-6-astra", false},
		{xai, "gpt-6-astra", false},
		{chatOnly, "gpt-6-astra", false},
	} {
		if got := takesEffortUpdates(tc.p, tc.model); got != tc.want {
			t.Errorf("%s %s: %v, want %v", tc.p.ID, tc.model, got, tc.want)
		}
	}
	for _, v := range []string{"", "off", "1"} {
		t.Setenv("MAGPIE_EFFORT_UPDATES", v)
		if takesEffortUpdates(codex, "gpt-6-luna") {
			t.Errorf("MAGPIE_EFFORT_UPDATES=%q still sends updates", v)
		}
	}
}

// #617: a Responses provider not OpenAI's gets a thread's effort change as
// the agent sent it.
func TestEffortChangeOtherVendor(t *testing.T) {
	t.Setenv("MAGPIE_EFFORT_UPDATES", "on")
	effortThreads.Lock()
	effortThreads.m, effortThreads.no = map[string]*effortThread{}, map[string]time.Time{}
	effortThreads.Unlock()
	f := &fake{t: t, reply: effortReply}
	setup(t, provider.Responses, f)
	for _, b := range []string{effortTurn("t1", "low", "a"), effortTurn("t1", "high", "a", "b")} {
		b = strings.Replace(b, "codex/gpt-6-luna", "fake/gpt-6-luna", 1)
		if code, body := post(t, "/v1/responses", b); code != 200 {
			t.Fatalf("status %d: %s", code, body)
		}
	}
	effort, _, input := sentShape(t, f.got)
	if effort != "high" || strings.Join(input, ",") != "user,assistant,user" {
		t.Fatalf("sent %q %v", effort, input)
	}
}

// ompTurn is omp's second turn of a GPT-6 conversation it began at low
// and has set to high since (#3918): the first effort stays at the top,
// and the change goes as a configuration_update before the user message
// it came with (planStableOpenAIEffort, omp's
// openai-configuration-update.ts).
func ompTurn(model string) string {
	return `{"model":"` + model + `","stream":true,"prompt_cache_key":"omp-1","reasoning":{"effort":"low"},"input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"a"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reply 1"}]},` +
		`{"type":"configuration_update","reasoning":{"effort":"high"}},` +
		`{"role":"user","content":[{"type":"input_text","text":"b"}]}]}`
}

// #3918 (lghnb): omp on magpie's codex/gpt-6.1-sol, a ChatGPT account,
// showed high while Routing said the model answered at low, the effort
// omp keeps at the top. The backend reasons at the update's effort, so
// the route, its try and the usage say that one, and the request goes on
// as omp sent it.
func TestClientEffortUpdateIsTheTurns(t *testing.T) {
	f := &fake{t: t, ctype: "none", reply: effortReply}
	effortCodexAccount(t, f)
	s := New()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(ompTurn("codex/gpt-6.1-sol"))))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if effort, _, input := sentShape(t, f.got); effort != "low" || strings.Join(input, ",") != "user,assistant,u:high,user" {
		t.Errorf("sent at %q, input %v", effort, input)
	}
	if r := lastRoute(s); r.Effort != "high" || len(r.Tries) != 1 || r.Tries[0].Effort != "high" {
		t.Errorf("route at %q, tries %+v", r.Effort, r.Tries)
	}
	if u := usage.Load(time.Time{}); len(u) != 1 || u[0].Effort != "high" {
		t.Errorf("usage %+v", u)
	}
}

// #3918: a client's configuration_update sets the turn's effort where it
// reaches the upstream: OpenAI's API, and a relay that takes Codex's
// requests as Codex sent them. Anyone else is sent the request without it
// (forVendor), or built in its own API from the effort at the top, and
// reasons at that one. The route keeps what the client asked; the try and
// the usage what the model was sent at.
func TestClientEffortUpdateByUpstream(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, p := range []provider.Provider{
		{ID: "oai", Name: "OpenAI", Key: "sk-k", Responses: "https://api.openai.com/v1", Models: []string{"gpt-6.1-sol"}},
		{ID: "asis", Name: "Relay as sent", Key: "k", CodexAsSent: true, Responses: "https://asis.test/v1", Models: []string{"gpt-6.1-sol"}},
		{ID: "relay", Name: "Relay", Key: "k", Responses: "https://relay.test/v1", Models: []string{"gpt-6.1-sol"}},
		{ID: "chatonly", Name: "Chat only", Key: "k", Chat: "https://chat.test/v1", Models: []string{"gpt-6.1-sol"}},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	up := &programsUpstream{}
	s := New()
	s.client = &http.Client{Transport: up}
	codex := http.Header{"User-Agent": {"codex_cli_rs/0.150.0 (Mac OS 26.2.0; arm64)"}, "Originator": {"codex_cli_rs"}}
	for _, c := range []struct {
		name, model string
		h           http.Header
		sent        string // the effort the try went at
		update      bool   // the item reached the upstream
	}{
		{"openai", "oai/gpt-6.1-sol", nil, "high", true},
		{"relay as sent, from codex", "asis/gpt-6.1-sol", codex, "high", true},
		{"relay as sent, not from codex", "asis/gpt-6.1-sol", nil, "low", false},
		{"relay", "relay/gpt-6.1-sol", codex, "low", false},
		{"chat upstream", "chatonly/gpt-6.1-sol", nil, "low", false},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(ompTurn(c.model)))
		for k, vs := range c.h {
			req.Header[k] = vs
		}
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
			continue
		}
		_, sent := up.last(t)
		r := lastRoute(s)
		if r.Effort != "high" || len(r.Tries) != 1 || r.Tries[0].Effort != c.sent || strings.Contains(string(sent), `"configuration_update"`) != c.update {
			t.Errorf("%s: route at %q, tries %+v; sent %s", c.name, r.Effort, r.Tries, sent)
		}
	}
	var got []string
	for _, rec := range usage.Load(time.Time{}) {
		got = append(got, rec.Effort)
	}
	if strings.Join(got, ",") != "high,high,low,low,low" {
		t.Errorf("usage efforts %q", got)
	}
}
