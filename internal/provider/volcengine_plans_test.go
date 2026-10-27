package provider

import (
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

// Volcengine Ark's plans have no list of their own to ask (an Agent Plan's
// /models answers 404, a Coding Plan's lists Ark's whole catalog), so a
// plan's models are the ones its region names, as the plan's overview lists
// them: both plans took deepseek-v4.1-flash-economy on 10-20 and
// kimi-k3.1-pro on 10-24, and the Agent Plan doubao-seedream-5.0-flash, to
// draw with, on 10-23 (#3932, yhong91: kimi-k3.1-pro couldn't be picked).
func TestVolcenginePlansOfferTheirNewModels(t *testing.T) {
	pr := Preset("volcengine")
	ids := func(ms []catalog.Model) (out []string) {
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	for _, plan := range []string{"coding", "agent"} {
		p, err := FromPreset("volcengine")
		if err != nil {
			t.Fatal(err)
		}
		r := pr.Regions[slices.IndexFunc(pr.Regions, func(r Region) bool { return r.ID == plan })]
		p.Chat, p.Responses, p.Anthropic = r.Chat, r.Responses, r.Anthropic
		models := ids(p.planModels(nil))
		for _, id := range []string{"kimi-k3.1-pro", "deepseek-v4.1-flash-economy"} {
			if !slices.Contains(models, id) {
				t.Errorf("%s plan: no %s among %v", plan, id, models)
			}
		}
		// an image model is drawn with, not offered to an agent to chat
		drawers := ids(p.PlanDrawers())
		if slices.Contains(drawers, "doubao-seedream-5.0-flash") != (plan == "agent") || slices.Contains(models, "doubao-seedream-5.0-flash") {
			t.Errorf("%s plan: draws with %v, chats with %v", plan, drawers, models)
		}
	}
}
