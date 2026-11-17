package provider_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// The fixtures' account id. No name or key may hold it or any part of an
// ARN.
const namesAccount = "123456789012"

const (
	namesLB        = "arn:aws:elasticloadbalancing:us-east-1:" + namesAccount + ":loadbalancer/app/web/0123456789abcdef"
	namesHTTP      = "arn:aws:elasticloadbalancing:us-east-1:" + namesAccount + ":listener/app/web/0123456789abcdef/1111111111111111"
	namesAlt       = "arn:aws:elasticloadbalancing:us-east-1:" + namesAccount + ":listener/app/web/0123456789abcdef/2222222222222222"
	namesHealth    = "arn:aws:elasticloadbalancing:us-east-1:" + namesAccount + ":listener-rule/app/web/0123456789abcdef/1111111111111111/3333333333333333"
	namesRule      = "arn:aws:elasticloadbalancing:us-east-1:" + namesAccount + ":listener-rule/app/web/0123456789abcdef/1111111111111111/4444444444444444"
	namesOtherRule = "arn:aws:elasticloadbalancing:us-east-1:" + namesAccount + ":listener-rule/app/api/5555555555555555/6666666666666666/7777777777777777"
	namesTG        = "arn:aws:elasticloadbalancing:us-east-1:" + namesAccount + ":targetgroup/app/8888888888888888"
	namesCert      = "arn:aws:acm:us-east-1:" + namesAccount + ":certificate/12345678-1234-1234-1234-123456789012"
)

type namesObject struct{ wire, id, state string }

func TestImportBulkNamesHoldNoARN(t *testing.T) {
	forward := `"action":[{"type":"forward","order":1,"target_group_arn":"` + namesTG + `"}]`
	listener := func(arn string, port string) namesObject {
		return namesObject{"aws_lb_listener", arn, `{"arn":"` + arn + `","id":"` + arn + `","load_balancer_arn":"` + namesLB + `","port":` + port + `,"protocol":"HTTP","default_action":[{"type":"forward","order":1,"target_group_arn":"` + namesTG + `"}]}`}
	}
	rule := func(arn, parent, priority, tags string) namesObject {
		return namesObject{"aws_lb_listener_rule", arn, `{"arn":"` + arn + `","id":"` + arn + `","listener_arn":"` + parent + `","priority":` + priority + `,` + forward + `,"condition":[{"path_pattern":[{"values":["/` + priority + `"]}]}]` + tags + `}`}
	}
	for _, tc := range []struct {
		name    string
		objects []namesObject
		// want maps each import id to the address bulk gives it.
		want map[string]string
	}{
		{
			name: "a load balancer and what it holds",
			objects: []namesObject{
				{"aws_lb", namesLB, `{"arn":"` + namesLB + `","id":"` + namesLB + `","name":"web","internal":true,"load_balancer_type":"application"}`},
				{"aws_lb_target_group", namesTG, `{"arn":"` + namesTG + `","id":"` + namesTG + `","name":"app","port":80,"protocol":"HTTP","target_type":"instance"}`},
				listener(namesHTTP, "80"),
				listener(namesAlt, "8080"),
				rule(namesHealth, namesHTTP, "10", `,"tags":{"Name":"health"}`),
				rule(namesRule, namesHTTP, "20", ""),
			},
			want: map[string]string{
				namesLB:     "aws_lb.aws_lb_web",
				namesTG:     "aws_lb_target_group.aws_lb_target_group_app",
				namesHTTP:   `aws_lb_listener.aws_lb_listener["web_http_80"]`,
				namesAlt:    `aws_lb_listener.aws_lb_listener["web_http_8080"]`,
				namesHealth: "aws_lb_listener_rule.aws_lb_listener_rule_web_http_80_health",
				namesRule:   "aws_lb_listener_rule.aws_lb_listener_rule_web_http_80_priority_20",
			},
		},
		{
			name:    "a rule whose listener is not imported",
			objects: []namesObject{rule(namesOtherRule, namesAlt, "5", "")},
			want:    map[string]string{namesOtherRule: "aws_lb_listener_rule.aws_lb_listener_rule_priority_5"},
		},
		{
			name:    "a listener whose load balancer is not imported",
			objects: []namesObject{listener(namesHTTP, "80")},
			want:    map[string]string{namesHTTP: "aws_lb_listener.aws_lb_listener_http_80"},
		},
		{
			name:    "a certificate",
			objects: []namesObject{{"aws_acm_certificate", namesCert, `{"arn":"` + namesCert + `","id":"` + namesCert + `","domain_name":"*.example.com"}`}},
			want:    map[string]string{namesCert: "aws_acm_certificate.aws_acm_certificate_example_com"},
		},
		{
			name:    "a certificate with no domain name",
			objects: []namesObject{{"aws_acm_certificate", namesCert, `{"arn":"` + namesCert + `","id":"` + namesCert + `"}`}},
			want:    map[string]string{namesCert: "aws_acm_certificate.aws_acm_certificate_1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &namesReader{objects: map[string]importer.Found{}}
			var selections []importer.Selection
			for _, o := range tc.objects {
				r, state := provider.ImportFixture(t, o.wire, o.state)
				reader.objects[o.id] = importer.Found{Schema: r, Source: schema.Source{Namespace: "hashicorp", Type: "aws"}, State: state}
				selections = append(selections, importer.Selection{Type: r.Name, IDs: []string{o.id}})
			}
			res, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: selections, Reader: reader})
			if err != nil {
				t.Fatal(err)
			}
			if res.Failed() {
				t.Fatalf("bulk failed: %v", res.Problems)
			}
			got := map[string]string{}
			for _, o := range res.Objects {
				got[o.ImportID] = o.Addr.String()
			}
			for id, want := range tc.want {
				if got[id] != want {
					t.Errorf("%s is %s, want %s", id, got[id], want)
				}
			}
			for _, imp := range res.Imports {
				names := []string{imp.Name}
				if imp.Key != nil {
					names = append(names, *imp.Key)
				}
				for _, name := range names {
					if leak := namesLeak(name); leak != "" {
						t.Errorf("name %q holds %s", name, leak)
					}
				}
			}
		})
	}
}

var namesDigits = regexp.MustCompile(`[0-9]{6,}`)

// namesLeak says what part of an ARN or account id name holds, or "".
// A cut name keeps only a part of the account id, so any long run of
// digits counts.
func namesLeak(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "arn:"), strings.Contains(lower, "arn_aws"):
		return "an ARN"
	case strings.Contains(lower, "elasticloadbalancing"), strings.Contains(lower, "us_east_1"), strings.Contains(lower, "us-east-1"):
		return "an ARN's service or region"
	case namesDigits.MatchString(name):
		return "a run of digits from an id"
	}
	return ""
}

// namesReader serves recorded objects and plans each one unchanged, so
// bulk writes their code with no provider.
type namesReader struct {
	objects map[string]importer.Found
}

func (r *namesReader) Read(_ context.Context, _ types.Name, _ addr.Addr, id string) (importer.Found, diag.List, error) {
	return r.objects[id], nil, nil
}

func (r *namesReader) Validator(types.Name, addr.Addr) importer.Validator {
	return func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil }
}

func (r *namesReader) Planner(_ types.Name, _ addr.Addr, prior importer.Found) importer.Planner {
	return func(context.Context, value.Value) (importer.Plan, error) {
		return importer.Plan{Value: prior.State}, nil
	}
}
