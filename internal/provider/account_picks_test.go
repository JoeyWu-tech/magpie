package provider

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"
)

// accountApart are the fields of Provider that a signed-in account or a
// plugin's provider doesn't take from the user, each with why: Save keeps
// none of them for one, and withPicks puts none of them back on it.
var accountApart = map[string]string{
	"ID":              "the entry's own: an account's picks are found by it",
	"Name":            "the sign-in's or its plugin's (account.go, pluginProviderServing)",
	"Icon":            "as Name",
	"Website":         "as Name",
	"Catalog":         "as Name",
	"Was":             "a subscription's id is its agent's, never renamed (Rename); Save keeps what the entry had",
	"Gave":            "as Was",
	"Preset":          "a preset's provider has a URL of its own, so it is one of the user's own (hasEndpoint)",
	"Chat":            "an account is asked where its sign-in says; an entry with a URL is a provider of the user's own (hasEndpoint)",
	"Responses":       "as Chat",
	"Anthropic":       "as Chat",
	"Gemini":          "as Chat",
	"Decide":          "as Chat",
	"BaseAPI":         "the API a custom provider's Base URL was given as",
	"Key":             "an account is signed in, not given a key; Kiro's alone is kept by Save, and read from the file (kiro.go)",
	"KeyName":         "a provider's keys: an account has its sign-in instead",
	"Keys":            "as KeyName",
	"KeyProtocol":     "as KeyName",
	"KeyWeight":       "as KeyName",
	"KeyPlan":         "as KeyName",
	"KeysURL":         "as KeyName",
	"AccessKeyID":     "a Volcengine or Tencent Cloud key's access key, for its plan's windows",
	"SecretAccessKey": "as AccessKeyID",
	"BalanceURL":      "a key's balance; an account's allowance is read from its vendor",
	"BalancePath":     "as BalanceURL",
	"BalanceMethod":   "as BalanceURL",
	"BalanceToken":    "as BalanceURL",
	"ZhipuTeam":       "a Zhipu or Z.ai key's team",
	"Vertex":          "Google Vertex AI's project and credentials, which a provider of the user's own is asked with",
	"ModelsURL":       "where a key's models are listed; an account's are its sign-in's or its plugin's",
	"RefreshEvery":    "refused for an account or a plugin's provider, whose list is fetched for it (errNotOwnList)",
	"AutoInclude":     "as RefreshEvery",
	"Headers":         "an account's requests go as its sign-in signs them (Account.sign), never with the user's headers",
	"AuthHeader":      "a third-party Anthropic API's key header; an account's auth is its sign-in's (BothAuth)",
	"PassUserAgent":   "an account goes as its own client (PassesUserAgent), and the CLI refuses user-agent= on one",
	"CodexAsSent":     "read only for a provider asked with a key (Account == nil): an account's requests are built as its client's",
	"Searches":        "a plugin's provider says it itself (pluginProviderServing), and another sign-in's is never read (searchesItself)",
	"Unredacted":      "only a provider on this computer or the local network goes unmasked (SkipsRedaction), never an account",
	"InsecureTLS":     "only a provider the user added with its own base URL takes it (TakesInsecureTLS)",
	"LimitRestOff":    "the editor offers it to a provider the user added alone, a relay whose limits pass in a moment (zoecats on Discord); an account's quota refusal is its vendor's word on it",
	"IconURL":         "an import link's, never stored",
	"Hidden":          "the entry's mark that the account was removed: Save keeps it (hiddenAccount), and All leaves such an account out",
	"Quiet":           "as Hidden, read from the entry by Hidden",
	"Tucked":          "as Quiet",
	"Account":         "the sign-in itself, never stored",
}

// Every setting a signed-in account or a plugin's provider takes from the
// user is put back on the account (withPicks), which is what Save keeps
// of it; every other field of Provider is in accountApart, with why. A
// field added later fails here until it is one or the other (#3743,
// tmuer: Save kept restCap, withPicks never put it back, and the gateway
// rested such an account 1, 2, 4, 8 and 16 minutes whatever the cap).
func TestAccountTakesEverySettingOrSaysWhyNot(t *testing.T) {
	var set Provider
	v := reflect.ValueOf(&set).Elem()
	fillEvery(v)
	got := reflect.ValueOf(withPicks(Provider{}, set))
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue // never stored
		}
		if v.Field(i).IsZero() {
			t.Fatalf("fillEvery gave %s no value: teach it a %s", f.Name, f.Type.Kind())
		}
		why, apart := accountApart[f.Name]
		switch carried := !got.Field(i).IsZero(); {
		case carried && apart:
			t.Errorf("%s is put back on an account (withPicks), but accountApart says an account doesn't take it: %s", f.Name, why)
		case !carried && !apart:
			t.Errorf("%s is neither put back on a signed-in account by withPicks, so Save drops it, nor in accountApart with why an account doesn't take it", f.Name)
		}
	}
	for name := range accountApart {
		if _, ok := v.Type().FieldByName(name); !ok {
			t.Errorf("accountApart names %s, which Provider hasn't", name)
		}
	}
}

// What the user sets on a signed-in account stays set and is the
// account's, each set as the user sets it: rest-cap and quota-rest as the
// CLI does, a Price rate as the account's editor saves it, patience= and
// additional-tools= as `magpie provider set` does, and an extraBody
// written into providers.json by hand (magpie-releases#18). Before, the
// account read none of the rests (#3743), the next save of it wrote them
// away, as every save did the extraBody, and the other three were never
// kept.
func TestAccountKeepsWhatIsSetOnIt(t *testing.T) {
	signIn(t)
	writeFile(t, Path(), map[string]any{"providers": []any{
		map[string]any{"id": "codex", "extraBody": map[string]any{"access_programs": []string{"daybreak"}}},
	}})
	if err := SetRestCap("codex", 3); err != nil {
		t.Fatal(err)
	}
	if err := SetQuotaRest("codex", 120); err != nil {
		t.Fatal(err)
	}
	p, err := Find("codex")
	if err != nil || p.Account == nil {
		t.Fatalf("codex: %+v %v", p, err)
	}
	p.PriceRate, p.Patience, p.AdditionalTools = 0.5, 120, true
	if err := Save(*p); err != nil {
		t.Fatal(err)
	}
	got, err := Find("codex")
	if err != nil {
		t.Fatal(err)
	}
	if most, out := got.RestCapOr(10*time.Minute), got.OwnQuotaRest(time.Now()); most != 3*time.Minute || out != 2*time.Hour {
		t.Errorf("the account rests at most %v, and %v out of quota; want 3m0s and 2h0m0s as set", most, out)
	}
	if got.PriceRate != 0.5 || got.Patience != 120 || !got.AdditionalTools {
		t.Errorf("price rate %v, patience %ds, additional tools %v; want 0.5, 120s and true as saved", got.PriceRate, got.Patience, got.AdditionalTools)
	}
	var programs []string
	json.Unmarshal(got.ExtraBody["access_programs"], &programs)
	if !slices.Equal(programs, []string{"daybreak"}) {
		t.Errorf("extraBody's access_programs after the account's saves: %s, want the [\"daybreak\"] written by hand", got.ExtraBody["access_programs"])
	}

	// the time of day, set last, stands over the minutes; off is the cap's too
	if err := SetQuotaResetAt("codex", "08:00"); err != nil {
		t.Fatal(err)
	}
	if err := SetRestCap("codex", RestCapOff); err != nil {
		t.Fatal(err)
	}
	if got, err = Find("codex"); err != nil {
		t.Fatal(err)
	}
	if got.QuotaResetAt != "08:00" || got.QuotaRest != 0 || !got.NoFailRest() {
		t.Errorf("quota back at %q, rest %d minutes, no rest after a failure %v; want 08:00, 0 and true", got.QuotaResetAt, got.QuotaRest, got.NoFailRest())
	}
}
