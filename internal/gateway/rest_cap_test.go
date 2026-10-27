package gateway

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

// #2008 (Gaudi-XIX): a provider's own rest cap (`magpie provider rest-cap`)
// is where a key that keeps failing, or keeps being rate limited as soon
// as its rest is over, stops doubling: past the gateway's 10 and 30
// minutes for a free key that is down for hours, or short of them.
func TestOwnRestCap(t *testing.T) {
	s := &Server{}
	near := func(d, want time.Duration) bool { return d > want-5*time.Second && d <= want }
	ladder := func(name string, capMin, status int, body string, want ...time.Duration) {
		t.Helper()
		c := candidate{p: provider.Provider{ID: "capped-" + name, Key: "k", RestCap: capMin}, model: "m", rest: "capped-" + name + "#k"}
		id := c.restKey()
		for i, w := range want {
			r := s.restAfter(c, status, http.Header{}, []byte(body))
			if !near(time.Until(r.Until), w) {
				t.Fatalf("%s #%d: rests %v by %s (%s), want %v", name, i+1, time.Until(r.Until), r.By, r.Why, w)
			}
			restingUntil.Lock()
			restingUntil.m[id] = time.Now().Add(-time.Second)
			restingUntil.Unlock()
		}
	}
	rate := `{"error":{"message":"Too many requests","type":"rate_limit_error"}}`
	fail := `{"error":{"message":"upstream connect error","type":"api_error"}}`
	m := time.Minute
	ladder("rate-long", 240, 429, rate, m, 2*m, 4*m, 8*m, 16*m, 32*m, 64*m, 128*m, 240*m, 240*m)
	ladder("rate-short", 3, 429, rate, m, 2*m, 3*m, 3*m)
	ladder("rate-own", 0, 429, rate, m, 2*m, 4*m, 8*m, 16*m, 30*m, 30*m)
	ladder("fail-long", 120, 502, fail, m, 2*m, 4*m, 8*m, 16*m, 32*m, 64*m, 120*m)
	ladder("fail-own", 0, 502, fail, m, 2*m, 4*m, 8*m, 10*m, 10*m)
}

func TestRestCapOr(t *testing.T) {
	if d := (provider.Provider{RestCap: 90}).RestCapOr(time.Minute); d != 90*time.Minute {
		t.Error(d)
	}
	if d := (provider.Provider{}).RestCapOr(time.Minute); d != time.Minute {
		t.Error(d)
	}
	if d := (provider.Provider{RestCap: 99999999}).RestCapOr(time.Minute); d != provider.MaxQuotaRest*time.Minute {
		t.Error(d)
	}
}

// Kismet Alan on Discord: a relay or account pool whose unknown errors
// come now and then, the next request often going through, is set never
// to rest after one (`magpie provider rest-cap <id> off`). Its 5xx and its
// dropped connection rest it no more, nor count toward a backoff; what
// says why still rests it as before: a rate limit (magpie's own 30
// minutes cap), out of quota, its key refused, its model not served.
func TestRestCapOff(t *testing.T) {
	s := &Server{}
	c := candidate{p: provider.Provider{ID: "nofail", Key: "k", RestCap: provider.RestCapOff}, model: "m", rest: "nofail#k"}
	id := c.restKey()
	t.Cleanup(func() {
		clearRest(id)
		clearRest(c.restID())
		routed.Lock()
		delete(routed.failures, id)
		delete(routed.failures, c.restID())
		routed.Unlock()
	})
	fail := `{"error":{"message":"upstream connect error","type":"api_error"}}`
	for i, status := range []int{500, 502, 503, 504, 520} {
		r := s.restAfter(c, status, http.Header{}, []byte(fail))
		if r.Why != failOther || r.By != "off" || r.Until.After(time.Now()) {
			t.Fatalf("#%d %d: rests %v by %s (%s), want no rest", i+1, status, time.Until(r.Until), r.By, r.Why)
		}
		if resting(c) {
			t.Fatalf("#%d %d: set aside", i+1, status)
		}
	}
	routed.Lock()
	n := routed.failures[id]
	routed.Unlock()
	if n != 0 {
		t.Fatalf("%d failures counted", n)
	}
	// what says why rests it still
	for _, k := range []struct {
		status int
		body   string
		why    string
	}{
		{401, `{"error":{"message":"invalid api key","type":"api_error"}}`, failOther},
		{403, `{"error":{"message":"forbidden","type":"api_error"}}`, failOther},
		{429, `{"error":{"message":"Too many requests","type":"rate_limit_error"}}`, failRate},
		{429, `{"error":{"message":"You exceeded your current quota, please check your plan","type":"insufficient_quota"}}`, failCredit},
		{404, `{"error":{"message":"The model m does not exist or you do not have access to it","type":"invalid_request_error"}}`, failOther},
	} {
		clearRest(id)
		clearRest(c.restID())
		r := s.restAfter(c, k.status, http.Header{}, []byte(k.body))
		if r.Why != k.why || r.By == "off" || !r.Until.After(time.Now()) || !resting(c) {
			t.Fatalf("%d %s: rests %v by %s (%s), want a rest as %s", k.status, k.body, time.Until(r.Until), r.By, r.Why, k.why)
		}
	}
	// a rate limit's cap is magpie's own 30 minutes, not none
	if d := c.p.RestCapOr(longestRateRest); d != longestRateRest {
		t.Fatalf("rate cap %v", d)
	}
}

// A signed-in provider's accounts rest no longer than the cap set on it
// (#3743, tmuer: a restCap the CLI wrote for a plugin's or a signed-in
// provider changed nothing, and its accounts went on resting 1, 2, 4, 8
// and 16 minutes, magpie's own). The candidates are those routing makes
// of it (perKeyBarred): the account Find gives, and the one on beside it
// (AlsoOn). Its quota-rest and rest-cap off reach them too.
func TestAccountRestsAtItsProvidersCap(t *testing.T) {
	codexSignedIn(t, "spare@example.com")
	restsAsSet(t, "codex", "gpt-5.5")
}

// The same for a plugin's provider of two accounts, the other kind the
// report names.
func TestPluginAccountRestsAtItsProvidersCap(t *testing.T) {
	bun := testenv.Bun(t)
	fresh(t)
	t.Setenv("MAGPIE_BUN", bun)
	t.Cleanup(plugin.Settle)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	abs, _ := filepath.Abs("../plugin/testdata/fake/index.js")
	if _, err := plugin.Add(ctx, abs); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k1", "k2"} {
		if _, err := provider.PluginAPIKey(ctx, "fakeco", 0, nil, k); err != nil {
			t.Fatal(err)
		}
	}
	restsAsSet(t, "fakeco", "fake-1")
}

// restsAsSet sets provider id's rest cap to 3 minutes and its quota-rest
// to 2 hours, as `magpie provider rest-cap` and `quota-rest` do, and
// checks that each account routing makes of it rests so: rate limited
// again as soon as its rest is over, 1, 2, 3 and 3 minutes; out of quota
// with no word of when, 2 hours; and with the cap off, failing without
// saying why, not at all.
func restsAsSet(t *testing.T, id, model string) {
	t.Helper()
	old, oldUp := allowances, codexMayBeUsedUp
	allowances = func(string) map[string]provider.Allowance { return nil } // no window read, none asked
	codexMayBeUsedUp = func(string) {}                                     // nor Codex's usage, out of quota
	t.Cleanup(func() { allowances, codexMayBeUsedUp = old, oldUp })
	accounts := func() []candidate {
		t.Helper()
		p, err := provider.Find(id)
		if err != nil || p.Account == nil {
			t.Fatalf("%s: %+v %v", id, p, err)
		}
		cs := []candidate{{p: *p, model: model, rest: p.ID}}
		for i, q := range p.AlsoOn() {
			cs = append(cs, candidate{p: q, model: model, rest: p.ID + "@" + q.Account.User, rank: i + 1})
		}
		if len(cs) != 2 {
			t.Fatalf("%s: %d accounts in use, want 2", id, len(cs))
		}
		t.Cleanup(func() {
			for _, c := range cs {
				clearRest(c.restKey())
				clearRest(c.restID())
			}
		})
		return cs
	}
	if err := provider.SetRestCap(id, 3); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetQuotaRest(id, 120); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	near := func(d, want time.Duration) bool { return d > want-5*time.Second && d <= want }
	rate := `{"error":{"message":"Too many requests","type":"rate_limit_error"}}`
	quota := `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`
	m := time.Minute
	for _, c := range accounts() {
		key := c.restKey()
		for i, want := range []time.Duration{m, 2 * m, 3 * m, 3 * m} {
			r := s.restAfter(c, 429, http.Header{}, []byte(rate))
			if !near(time.Until(r.Until), want) {
				t.Fatalf("%s rate limited #%d: rests %v by %s, want %v (its provider's cap is 3m)", key, i+1, time.Until(r.Until), r.By, want)
			}
			restingUntil.Lock()
			restingUntil.m[key] = time.Now().Add(-time.Second) // over, and limited again at once
			restingUntil.Unlock()
		}
		clearRest(key)
		if r := s.restAfter(c, 429, http.Header{}, []byte(quota)); r.Why != failQuota || !near(time.Until(r.Until), 2*time.Hour) {
			t.Fatalf("%s out of quota: rests %v by %s (%s), want 2h0m0s as its quota-rest says", key, time.Until(r.Until), r.By, r.Why)
		}
	}
	if err := provider.SetRestCap(id, provider.RestCapOff); err != nil {
		t.Fatal(err)
	}
	fail := `{"error":{"message":"upstream connect error","type":"api_error"}}`
	for _, c := range accounts() {
		if r := s.restAfter(c, 502, http.Header{}, []byte(fail)); r.By != "off" || r.Until.After(time.Now()) {
			t.Fatalf("%s failing with the cap off: rests %v by %s, want no rest", c.restKey(), time.Until(r.Until), r.By)
		}
	}
}
