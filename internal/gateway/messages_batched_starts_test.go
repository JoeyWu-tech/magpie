package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/claudeseal"
	"github.com/yetone/magpie/internal/provider"
)

// mapleleo-hub's upstream (#3945), an aggregator speaking Anthropic
// Messages, starts every block of a turn before any block's input: five
// read_file calls with an empty text block between each two, then each
// call's input and stop in index order. Read one after another, the first
// four calls reached Codex with "{}" and the fifth with all five inputs
// run together, and Codex failed every call.

// readCalls are the reply's calls as the report names them, with the file
// each one reads.
var readCalls = []struct{ id, path string }{
	{"call_9eaffda9", "a.py"}, {"call_a43903f1", "b.py"}, {"call_89a34009", "c.py"},
	{"call_1abdee88", "d.py"}, {"call_a5fa213f", "e.py"},
}

// Lines of a Messages stream: a block's start, a delta of it and its
// stop; a read_file call's start, a piece of a call's input, a piece of
// text; the reply's end.
func blockStart(i int, block string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, block)
}

func blockDelta(i int, delta string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, delta)
}

func blockStop(i int) string {
	return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i)
}

func readStart(i int, id string) string {
	return blockStart(i, `{"type":"tool_use","id":"`+id+`","name":"read_file","input":{}}`)
}

func inputDelta(i int, partial string) string {
	return blockDelta(i, `{"type":"input_json_delta","partial_json":`+jsonStr(partial)+`}`)
}

func textDelta(i int, s string) string {
	return blockDelta(i, `{"type":"text_delta","text":`+jsonStr(s)+`}`)
}

const (
	toolUseEnd  = `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":96}}`
	messageStop = `{"type":"message_stop"}`
)

// emptyHead is the reply's start as the report shows it: a thinking block
// and a text block, each started and stopped.
func emptyHead() []string {
	return []string{anthStart("claude-opus-4-6", 120),
		blockStart(0, `{"type":"thinking","thinking":"","signature":""}`), blockStop(0),
		blockStart(1, `{"type":"text","text":""}`), blockStop(1)}
}

// startsOf are the five calls' starts at blocks 2, 4, 6, 8 and 10, with a
// text block between call n and the next, saying between(n) ("" says
// nothing, as the report's do).
func startsOf(between func(n int) string) []string {
	var lines []string
	for n, c := range readCalls {
		lines = append(lines, readStart(2+2*n, c.id))
		if n < len(readCalls)-1 {
			i := 3 + 2*n
			lines = append(lines, blockStart(i, `{"type":"text","text":""}`))
			if s := between(n); s != "" {
				lines = append(lines, textDelta(i, s))
			}
			lines = append(lines, blockStop(i))
		}
	}
	return lines
}

func saysNothing(int) string { return "" }

// inputLines is call n's input, whole or in the pieces given, and its
// block's stop.
func inputLines(n int, pieces ...string) []string {
	if len(pieces) == 0 {
		pieces = []string{`{"path": "` + readCalls[n].path + `"}`}
	}
	var lines []string
	for _, p := range pieces {
		lines = append(lines, inputDelta(2+2*n, p))
	}
	return append(lines, blockStop(2+2*n))
}

// reportedOrder is the reply in the order the report shows its upstream
// sending it.
func reportedOrder() []string {
	lines := append(emptyHead(), startsOf(saysNothing)...)
	for n := range readCalls {
		lines = append(lines, inputLines(n)...)
	}
	return append(lines, toolUseEnd, messageStop)
}

const readFileTool = `"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`

// readFilesResponses and readFilesChat ask for the five files, as Codex
// and as a Chat client do.
const readFilesResponses = `{"model":"up/claude-opus-4-6","stream":true,"input":"Read a.py to e.py.","tools":[{"type":"function",` + readFileTool + `}]}`

func readFilesChat(stream bool) string {
	return fmt.Sprintf(`{"model":"up/claude-opus-4-6","stream":%t,"messages":[{"role":"user","content":"Read a.py to e.py."}],"tools":[{"type":"function","function":{%s}}]}`, stream, readFileTool)
}

// messagesReply serves claude-opus-4-6 from an Anthropic upstream that
// answers with lines as they are, and returns what a client asking path
// with body got.
func messagesReply(t *testing.T, path, body string, lines []string) string {
	t.Helper()
	fresh(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType(l), l)
		}
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "up", Name: "UP", Key: "k", Models: []string{"claude-opus-4-6"}, Anthropic: up.URL}); err != nil {
		t.Fatal(err)
	}
	code, out := post(t, path, body)
	if code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	return out
}

// callOut is a call as a client got it; streamed is what of its arguments
// came in deltas, for a Responses item.
type callOut struct{ id, name, args, streamed string }

// callsText is calls as "id name arguments" lines.
func callsText(calls []callOut) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "%s %s %s\n", c.id, c.name, c.args)
	}
	return b.String()
}

// readsIn is readCalls n, in the order given (all five in turn when none
// is), each with its own input, as callsText writes them.
func readsIn(order ...int) string {
	if len(order) == 0 {
		order = []int{0, 1, 2, 3, 4}
	}
	var calls []callOut
	for _, n := range order {
		calls = append(calls, callOut{id: readCalls[n].id, name: "read_file", args: `{"path": "` + readCalls[n].path + `"}`})
	}
	return callsText(calls)
}

// responsesCallsOf is a Responses stream's function_call items, in output
// order. Each item's deltas and its done must be at the output_index it
// was added at, and the stream must complete.
func responsesCallsOf(t *testing.T, body string) []callOut {
	t.Helper()
	var calls []callOut
	at := map[string]any{} // an item's output_index, by its id
	streamed := map[string]string{}
	completed := false
	for _, ev := range events(body) {
		switch ev["type"] {
		case "response.output_item.added":
			if it, _ := ev["item"].(map[string]any); it["type"] == "function_call" {
				id, _ := it["id"].(string)
				at[id] = ev["output_index"]
			}
		case "response.function_call_arguments.delta":
			id, _ := ev["item_id"].(string)
			if i, ok := at[id]; !ok || i != ev["output_index"] {
				t.Fatalf("arguments of an item not added at their output_index: %v\n%s", ev, body)
			}
			d, _ := ev["delta"].(string)
			streamed[id] += d
		case "response.output_item.done":
			it, _ := ev["item"].(map[string]any)
			if it["type"] != "function_call" {
				continue
			}
			id, _ := it["id"].(string)
			if i, ok := at[id]; !ok || i != ev["output_index"] {
				t.Fatalf("an item done away from its output_index: %v\n%s", ev, body)
			}
			c := callOut{streamed: streamed[id]}
			c.id, _ = it["call_id"].(string)
			c.name, _ = it["name"].(string)
			c.args, _ = it["arguments"].(string)
			calls = append(calls, c)
		case "response.completed":
			completed = true
		}
	}
	if !completed {
		t.Fatalf("no response.completed:\n%s", body)
	}
	return calls
}

// chatStreamCalls is a Chat stream's tool calls by their index: the id and
// name their first fragment gave, the arguments of every fragment.
func chatStreamCalls(body string) []callOut {
	var calls []callOut
	for _, ev := range events(body) {
		for _, c := range choices(ev) {
			d, _ := c["delta"].(map[string]any)
			parts, _ := d["tool_calls"].([]any)
			for _, x := range parts {
				tc, _ := x.(map[string]any)
				i, _ := tc["index"].(float64)
				for len(calls) <= int(i) {
					calls = append(calls, callOut{})
				}
				c := &calls[int(i)]
				if id, _ := tc["id"].(string); id != "" {
					c.id = id
				}
				fn, _ := tc["function"].(map[string]any)
				if name, _ := fn["name"].(string); name != "" {
					c.name = name
				}
				args, _ := fn["arguments"].(string)
				c.args += args
			}
		}
	}
	return calls
}

// The reply as Codex gets it: five function_call items, each with its own
// call_id and input, streamed at an output_index of its own.
func TestBatchedStartsToResponses(t *testing.T) {
	body := messagesReply(t, "/v1/responses", readFilesResponses, reportedOrder())
	calls := responsesCallsOf(t, body)
	if got, want := callsText(calls), readsIn(); got != want {
		t.Fatalf("function_call items:\n%swant:\n%s", got, want)
	}
	for _, c := range calls {
		if c.streamed != c.args {
			t.Errorf("%s streamed %q, done with %q", c.id, c.streamed, c.args)
		}
	}
}

// The same reply to a Chat client, streamed and whole: five tool calls,
// each with its own input.
func TestBatchedStartsToChat(t *testing.T) {
	body := messagesReply(t, "/v1/chat/completions", readFilesChat(true), reportedOrder())
	if got, want := callsText(chatStreamCalls(body)), readsIn(); got != want {
		t.Fatalf("streamed tool calls:\n%swant:\n%s", got, want)
	}
	if got := chatFinish(body); got != "tool_calls" {
		t.Errorf("finish_reason %q, want tool_calls", got)
	}
	body = messagesReply(t, "/v1/chat/completions", readFilesChat(false), reportedOrder())
	var whole struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &whole); err != nil || len(whole.Choices) != 1 {
		t.Fatalf("whole reply %s: %v", body, err)
	}
	var calls []callOut
	for _, c := range whole.Choices[0].Message.ToolCalls {
		calls = append(calls, callOut{id: c.ID, name: c.Function.Name, args: c.Function.Arguments})
	}
	if got, want := callsText(calls), readsIn(); got != want {
		t.Fatalf("whole reply's tool calls:\n%swant:\n%s", got, want)
	}
}

// readFilesGemini asks for the five files as Gemini CLI does.
const readFilesGemini = `{"contents":[{"role":"user","parts":[{"text":"Read a.py to e.py."}]}],"tools":[{"functionDeclarations":[{` + readFileTool + `}]}]}`

// And to a Gemini client: five functionCall parts, each with its own id
// and args.
func TestBatchedStartsToGemini(t *testing.T) {
	body := messagesReply(t, "/v1beta/models/up/claude-opus-4-6:streamGenerateContent?alt=sse", readFilesGemini, reportedOrder())
	var got, want []string
	for _, ev := range events(body) {
		cands, _ := ev["candidates"].([]any)
		for _, c := range cands {
			cand, _ := c.(map[string]any)
			content, _ := cand["content"].(map[string]any)
			parts, _ := content["parts"].([]any)
			for _, p := range parts {
				part, _ := p.(map[string]any)
				if fc, ok := part["functionCall"].(map[string]any); ok {
					args, _ := json.Marshal(fc["args"])
					got = append(got, fmt.Sprintf("%v %v %s", fc["id"], fc["name"], args))
				}
			}
		}
	}
	for _, c := range readCalls {
		want = append(want, c.id+` read_file {"path":"`+c.path+`"}`)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("functionCall parts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// thinkingHead is the reply's start with something in it: Claude's
// thinking, signed, and a line of text.
func thinkingHead() []string {
	return []string{anthStart("claude-opus-4-6", 120),
		blockStart(0, `{"type":"thinking","thinking":"","signature":""}`),
		blockDelta(0, `{"type":"thinking_delta","thinking":"Five files, read them at once."}`),
		blockDelta(0, `{"type":"signature_delta","signature":"EqQBCgIYAhIMbatched"}`),
		blockStop(0),
		blockStart(1, `{"type":"text","text":""}`), textDelta(1, "Reading the five files."), blockStop(1)}
}

// thenNext is a text block between two calls naming the next file.
func thenNext(n int) string { return " Then " + readCalls[n+1].path + "." }

// severalPieces is call n's input cut in pieces as Anthropic's own stream
// cuts one, an empty piece first.
func severalPieces(n int) []string {
	p := readCalls[n].path
	return []string{"", `{"pa`, `th": "`, p[:3], p[3:] + `"}`}
}

// Other upstreams that start the calls before their input: each call
// still reaches Codex with its own input, whatever order the input comes
// in, and the thinking and text around them are kept.
func TestBatchedStartsVariants(t *testing.T) {
	type check func(t *testing.T, body string, calls []callOut)
	inOrder := func(order ...int) check {
		return func(t *testing.T, _ string, calls []callOut) {
			if got, want := callsText(calls), readsIn(order...); got != want {
				t.Fatalf("function_call items:\n%swant:\n%s", got, want)
			}
		}
	}
	for _, c := range []struct {
		name  string
		lines func() []string
		check check
	}{
		{"thinking first, text between the calls", func() []string {
			lines := append(thinkingHead(), startsOf(thenNext)...)
			for n := range readCalls {
				lines = append(lines, inputLines(n)...)
			}
			return append(lines, toolUseEnd, messageStop)
		}, func(t *testing.T, body string, calls []callOut) {
			inOrder()(t, body, calls)
			var thought, sealed, said string
			for _, ev := range events(body) {
				if ev["type"] != "response.output_item.done" {
					continue
				}
				it, _ := ev["item"].(map[string]any)
				switch it["type"] {
				case "reasoning":
					summary, _ := it["summary"].([]any)
					for _, s := range summary {
						part, _ := s.(map[string]any)
						text, _ := part["text"].(string)
						thought += text
					}
					enc, _ := it["encrypted_content"].(string)
					if block, ours := claudeseal.Open(enc); ours {
						sealed = string(block)
					}
				case "message":
					content, _ := it["content"].([]any)
					for _, x := range content {
						part, _ := x.(map[string]any)
						text, _ := part["text"].(string)
						said += text
					}
				}
			}
			// the signature stays with the thinking it signed, for the
			// next request to give back whole
			if want := `{"type":"thinking","thinking":"Five files, read them at once.","signature":"EqQBCgIYAhIMbatched"}`; thought != "Five files, read them at once." || sealed != want {
				t.Errorf("reasoning %q sealing %s, want its block %s", thought, sealed, want)
			}
			if want := "Reading the five files. Then b.py. Then c.py. Then d.py. Then e.py."; said != want {
				t.Errorf("text %q, want %q", said, want)
			}
		}},
		{"input in several deltas", func() []string {
			lines := append(emptyHead(), startsOf(saysNothing)...)
			for n := range readCalls {
				lines = append(lines, inputLines(n, severalPieces(n)...)...)
			}
			return append(lines, toolUseEnd, messageStop)
		}, inOrder()},
		{"input out of index order", func() []string {
			// each call goes as its input comes
			lines := append(emptyHead(), startsOf(saysNothing)...)
			for _, n := range []int{2, 0, 4, 1, 3} {
				lines = append(lines, inputLines(n)...)
			}
			return append(lines, toolUseEnd, messageStop)
		}, inOrder(2, 0, 4, 1, 3)},
		{"input by turns, stops last", func() []string {
			// every call's first half, then every second half, then the
			// stops, the last call's first: one call streams, the rest
			// wait for it and go when it stops, in index order
			lines := append(emptyHead(), startsOf(saysNothing)...)
			for n := range readCalls {
				lines = append(lines, inputDelta(2+2*n, `{"path": `))
			}
			for n, c := range readCalls {
				lines = append(lines, inputDelta(2+2*n, `"`+c.path+`"}`))
			}
			for n := len(readCalls) - 1; n >= 0; n-- {
				lines = append(lines, blockStop(2+2*n))
			}
			return append(lines, toolUseEnd, messageStop)
		}, inOrder()},
		{"no block stops, starts and input out of order", func() []string {
			// no content_block_stop and no message_stop: the call whose
			// input came first streams, the others go when the reply
			// ends, in index order
			lines := []string{anthStart("claude-opus-4-6", 120)}
			for n := len(readCalls) - 1; n >= 0; n-- {
				lines = append(lines, readStart(2+2*n, readCalls[n].id))
			}
			for _, n := range []int{2, 0, 4, 1, 3} {
				lines = append(lines, inputDelta(2+2*n, `{"path": "`+readCalls[n].path+`"}`))
			}
			return append(lines, toolUseEnd)
		}, inOrder(2, 0, 1, 3, 4)},
		{"a server tool among the calls", func() []string {
			lines := append(emptyHead(), startsOf(saysNothing)...)
			lines = append(lines,
				blockStart(11, `{"type":"server_tool_use","id":"srvtoolu_01","name":"web_search","input":{}}`),
				inputDelta(11, `{"query": "python files"}`), blockStop(11),
				blockStart(12, `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_01","content":[]}`), blockStop(12))
			for n := range readCalls {
				lines = append(lines, inputLines(n)...)
			}
			return append(lines, toolUseEnd, messageStop)
		}, inOrder()},
		{"calls whole in their starts, around one streamed", func() []string {
			// a start carrying its call's whole input (蓝猫 on Discord)
			// goes at once while no call is open, and waits for the open
			// one to stop while one is
			whole := func(n int) []string {
				c := readCalls[n]
				return []string{blockStart(2+2*n, `{"type":"tool_use","id":"`+c.id+`","name":"read_file","input":{"path": "`+c.path+`"}}`), blockStop(2 + 2*n)}
			}
			lines := append(emptyHead(), readStart(2, readCalls[0].id))
			lines = append(lines, whole(1)...)
			lines = append(lines, inputDelta(2, `{"path": `))
			lines = append(lines, whole(2)...)
			lines = append(lines, whole(3)...)
			lines = append(lines, inputDelta(2, `"a.py"}`), blockStop(2))
			lines = append(lines, whole(4)...)
			return append(lines, toolUseEnd, messageStop)
		}, inOrder(1, 0, 2, 3, 4)},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := messagesReply(t, "/v1/responses", readFilesResponses, c.lines())
			calls := responsesCallsOf(t, body)
			c.check(t, body, calls)
			for _, x := range calls {
				if x.streamed != x.args {
					t.Errorf("%s streamed %q, done with %q", x.id, x.streamed, x.args)
				}
			}
		})
	}
}

// readLines reads lines through the Messages decoder and returns every
// event it gave. What a line says on its own (decodeAnthropic of it
// alone) must go out while that line is read, at the end of what does: a
// call's start is the one line excepted, as it goes out with the call's
// input.
func readLines(t *testing.T, lines []string) []Event {
	t.Helper()
	dec := decoder(provider.Anthropic)
	var all []Event
	for i, l := range lines {
		var got, own []Event
		if err := dec(l, func(e Event) { got = append(got, e) }); err != nil {
			t.Fatal(err)
		}
		if err := decodeAnthropic(l, func(e Event) { own = append(own, e) }); err != nil {
			t.Fatal(err)
		}
		var start struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		json.Unmarshal([]byte(l), &start)
		callStart := start.Type == "content_block_start" && start.ContentBlock.Type == "tool_use"
		if !callStart && len(own) > 0 && (len(got) < len(own) || !reflect.DeepEqual(got[len(got)-len(own):], own)) {
			t.Errorf("line %d %s\ngave %s, want what it says itself, %s, to go out with it", i, l, eventsText(got), eventsText(own))
		}
		all = append(all, got...)
	}
	return all
}

// eventsText is events as their kinds and what they carry.
func eventsText(evs []Event) string {
	names := map[EventKind]string{KStart: "start", KText: "text", KThink: "think", KSig: "sig", KToolStart: "tool_start",
		KToolArgs: "tool_args", KStop: "stop", KUsage: "usage", KError: "error", KThinkStart: "think_start", KSealed: "sealed"}
	var out []string
	for _, e := range evs {
		s := names[e.Kind]
		if s == "" {
			s = fmt.Sprint(e.Kind)
		}
		out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %s %s %s", s, e.ID, e.Name, e.Text, e.Stop)))
	}
	return "[" + strings.Join(out, " | ") + "]"
}

// Anthropic's own order, each block started, given its deltas and stopped
// before the next starts (the tool use stream of its streaming docs, an
// empty piece of input first), comes out of the decoder event for event
// as each line read on its own does, as before the calls were read by
// their index. Only when it goes out moves: a call's start, with its
// first input, the line after, or with its stop when it has none.
func TestMessagesOwnOrderUnchanged(t *testing.T) {
	lines := []string{anthStart("claude-opus-4-6", 472),
		blockStart(0, `{"type":"thinking","thinking":"","signature":""}`),
		blockDelta(0, `{"type":"thinking_delta","thinking":"Two cities, and the time."}`),
		blockDelta(0, `{"type":"signature_delta","signature":"EqQBCgIYAhIMowned"}`),
		blockStop(0),
		blockStart(1, `{"type":"text","text":""}`),
		textDelta(1, "Okay, let's check the weather for San Francisco, CA:"),
		blockStop(1),
		blockStart(2, `{"type":"tool_use","id":"toolu_01T1x1fJ34qAmk2tNTrN7Up6","name":"get_weather","input":{}}`),
		inputDelta(2, ""), inputDelta(2, `{"location":`), inputDelta(2, ` "San`), inputDelta(2, ` Francisc`), inputDelta(2, `o,`), inputDelta(2, ` CA"}`),
		blockStop(2),
		blockStart(3, `{"type":"tool_use","id":"toolu_02","name":"get_time","input":{}}`),
		blockStop(3),
		blockStart(4, `{"type":"text","text":""}`), textDelta(4, " And New York:"), blockStop(4),
		blockStart(5, `{"type":"tool_use","id":"toolu_03","name":"get_weather","input":{}}`),
		inputDelta(5, ""), inputDelta(5, `{"location": "New York, NY"}`),
		blockStop(5),
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":89}}`,
		messageStop,
	}
	got := readLines(t, lines)
	var want []Event
	for _, l := range lines {
		decodeAnthropic(l, func(e Event) { want = append(want, e) })
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n%s\nwant, as each line read on its own:\n%s", eventsText(got), eventsText(want))
	}
}

// In the batched order the decoder holds back only calls: the thinking
// and the text go out with the line that carries them, and each call at
// its first input, streaming from there. A client given the events gets
// the thinking, signed, the text, and five calls, each with its own input.
func TestBatchedStartsHoldOnlyCalls(t *testing.T) {
	lines := append(thinkingHead(), startsOf(thenNext)...)
	for n := range readCalls {
		lines = append(lines, inputLines(n, severalPieces(n)...)...)
	}
	lines = append(lines, toolUseEnd, messageStop)
	var col collector
	for _, e := range readLines(t, lines) {
		col.add(e)
	}
	res := col.finish()
	var got []string
	for _, p := range res.Parts {
		switch p.Kind {
		case Thinking:
			got = append(got, "thinking "+p.Text+" signed "+p.Signature)
		case Text:
			got = append(got, "text "+p.Text)
		case ToolCall:
			got = append(got, p.ID+" "+p.Name+" "+string(p.Args))
		}
	}
	want := []string{"thinking Five files, read them at once. signed EqQBCgIYAhIMbatched",
		"text Reading the five files. Then b.py. Then c.py. Then d.py. Then e.py."}
	want = append(want, strings.Split(strings.TrimSuffix(readsIn(), "\n"), "\n")...)
	if !slices.Equal(got, want) {
		t.Fatalf("parts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if res.Stop != "tool" {
		t.Errorf("stop %q, want tool", res.Stop)
	}
}
