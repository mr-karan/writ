package ui_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/policy"
	"github.com/mr-karan/writ/internal/server"
)

// An OpenTofu plan's page shows the policy's decision: a deny says the
// plan cannot be approved and offers no approval, and each result shows
// its set, rule, and masked message.
func TestTofuPlanPolicy(t *testing.T) {
	examples, err := policy.LoadDir("../../examples/policy")
	if err != nil {
		t.Fatal(err)
	}
	x := newTofuUI(t, func(c *server.Config) {
		c.Policies = &server.Policies{Timeout: time.Minute, Memory: policy.DefaultMemory, Sets: []server.PolicySet{
			{Name: "examples", Level: policy.Mandatory, Files: examples},
			{Name: "forgets", Level: policy.Mandatory, Files: map[string]string{"forget.rego": `package main

import rego.v1

deny contains sprintf("%s is forgotten, with keepers %v", [rc.address, rc.change.before.keepers]) if {
	some rc in input.resource_changes
	rc.change.actions == ["forget"]
}
`}},
		}}
	})
	x.tofu.set(second)
	status, body := x.post("/v1/plans", map[string]any{"format": "writ.plan-request/v1", "stacks": []string{"app"}, "source": map[string]any{"repo": "demo", "commit": x.commit}})
	if status != http.StatusAccepted {
		t.Fatalf("plan: %d %s", status, body)
	}
	var created, p server.RemotePlan
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	x.wait("/v1/plans/"+created.Plan.Ref, "plan", "planning", &p)
	if p.Plan.Status != "refused" || p.Plan.ID == "" {
		t.Fatalf("plan %+v\n%s", p.Plan, p.Text)
	}
	page, doc := x.page("/ui/plans/" + p.Plan.ID)
	section := words(byID(t, doc, "h-policy").Parent)
	for _, want := range []string{
		"A policy denies this plan, so it cannot be approved or applied.",
		"deny forgets deny random_pet.gone is forgotten, with keepers",
		"warn examples warn_replace random_pet.rotated is replaced: OpenTofu deletes it and creates it again",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the policy section lacks %q:\n%s", want, section)
		}
	}
	if strings.Contains(page, "Approve this plan") {
		t.Error("the page of a plan a policy denied offers an approval")
	}
	for _, c := range []string{canaryFirst, canarySecond} {
		if strings.Contains(page, c) || strings.Contains(p.Text, c) || strings.Contains(string(p.Policy), c) {
			t.Fatalf("the plan's page, text, or decision holds the canary %s", c)
		}
	}
}
