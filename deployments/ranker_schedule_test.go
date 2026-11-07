package deployments

// The ranker (top blocked/allowed lists) runs on a configurable schedule (owner, 2026-11-07): hourly by default, or
// for example hourly during office hours on weekdays in local time. EventBridge rules only know UTC, so the schedule
// is an EventBridge Scheduler schedule with a time zone.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRankerSchedule_IsConfigurableWithATimeZone(t *testing.T) {
	module := filesIn(loadTerraform(t, santaAPIDir), santaAPIDir)

	for name, want := range map[string]string{"ranker_schedule_expression": `"rate(1 hour)"`, "ranker_schedule_timezone": `"UTC"`} {
		if d, ok := variableDefault(t, module, name); !ok || d != want {
			t.Errorf("variable %s default %s, want %s", name, d, want)
		}
	}

	if rules := blocks(module, "resource", "aws_cloudwatch_event_rule"); len(rules) != 0 {
		t.Errorf("an EventBridge rule still schedules something (%s); the ranker must have exactly one schedule", rules[0].Labels[1])
	}

	schedules := blocks(module, "resource", "aws_scheduler_schedule")
	if len(schedules) != 1 {
		t.Fatalf("found %d aws_scheduler_schedule resources, want 1", len(schedules))
	}
	s := schedules[0]
	for attr, want := range map[string]string{"schedule_expression": "var.ranker_schedule_expression", "schedule_expression_timezone": "var.ranker_schedule_timezone"} {
		a, ok := s.Body.Attributes[attr]
		if !ok || strings.TrimSpace(sourceOf(t, module, a.Expr)) != want {
			t.Errorf("schedule %s is not %s", attr, want)
		}
	}
	if a, ok := s.Body.Attributes["name"]; !ok || !strings.Contains(sourceOf(t, module, a.Expr), "var.prefix") {
		t.Error("schedule name does not include var.prefix")
	}
	windows := nested(s, "flexible_time_window")
	if len(windows) != 1 {
		t.Fatal("schedule has no flexible_time_window")
	}
	if v, _ := literal(windows[0].Body.Attributes["mode"]); v != "OFF" {
		t.Errorf("flexible_time_window mode %q, want OFF", v)
	}
	targets := nested(s, "target")
	if len(targets) != 1 {
		t.Fatalf("schedule has %d targets, want 1", len(targets))
	}
	target := targets[0]
	if a, ok := target.Body.Attributes["arn"]; !ok || !strings.Contains(sourceOf(t, module, a.Expr), "aws_lambda_function.ranker") {
		t.Error("schedule target is not the ranker function")
	}
	// The next run rebuilds every list, so a failed run is not retried (as before).
	retries := nested(target, "retry_policy")
	if len(retries) != 1 {
		t.Error("schedule target has no retry_policy")
	} else if a, ok := retries[0].Body.Attributes["maximum_retry_attempts"]; !ok || strings.TrimSpace(sourceOf(t, module, a.Expr)) != "0" {
		t.Error("schedule target retries; want maximum_retry_attempts = 0")
	}
	role, ok := target.Body.Attributes["role_arn"]
	if !ok {
		t.Fatal("schedule target has no role_arn")
	}
	roleSrc := sourceOf(t, module, role.Expr)
	if !strings.HasPrefix(strings.TrimSpace(roleSrc), "aws_iam_role.") {
		t.Fatalf("schedule role_arn = %s, want a role of this module", roleSrc)
	}

	// The scheduler's role may invoke the ranker and nothing else.
	var all strings.Builder
	for _, f := range module {
		all.Write(f.src)
	}
	src := all.String()
	if !strings.Contains(src, "scheduler.amazonaws.com") {
		t.Error("no role trusts scheduler.amazonaws.com")
	}
	if !strings.Contains(src, "lambda:InvokeFunction") {
		t.Error("no policy allows lambda:InvokeFunction for the scheduler")
	}
	// Only the policies attached to the scheduler's own role: other roles (eventupload's optional output Lambda)
	// may invoke other functions.
	roleName := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(roleSrc), "aws_iam_role."), ".arn")
	attached := 0
	for _, rp := range blocks(module, "resource", "aws_iam_role_policy") {
		r, ok := rp.Body.Attributes["role"]
		if !ok || !strings.Contains(sourceOf(t, module, r.Expr), "aws_iam_role."+roleName+".") {
			continue
		}
		attached++
		pol, ok := rp.Body.Attributes["policy"]
		if !ok {
			continue
		}
		polSrc := sourceOf(t, module, pol.Expr)
		for _, p := range blocks(module, "data", "aws_iam_policy_document") {
			if !strings.Contains(polSrc, "data.aws_iam_policy_document."+p.Labels[1]+".") {
				continue
			}
			body := string(p.Body.SrcRange.SliceBytes(srcOf(t, module, p)))
			if strings.Contains(body, `"*"`) || !strings.Contains(body, "aws_lambda_function.ranker") {
				t.Errorf("the scheduler role's policy %s allows more than invoking the ranker:\n%s", p.Labels[1], body)
			}
		}
	}
	if attached == 0 {
		t.Errorf("no aws_iam_role_policy is attached to aws_iam_role.%s", roleName)
	}
	if n := len(blocks(module, "resource", "aws_iam_role_policy_attachment")); n > 0 {
		for _, a := range blocks(module, "resource", "aws_iam_role_policy_attachment") {
			if r, ok := a.Body.Attributes["role"]; ok && strings.Contains(sourceOf(t, module, r.Expr), "aws_iam_role."+roleName+".") {
				t.Errorf("a managed policy is attached to the scheduler role (%s)", a.Labels[1])
			}
		}
	}
	if len(blocks(module, "resource", "aws_lambda_permission")) != 0 {
		t.Error("an aws_lambda_permission is left over from the EventBridge rule; the scheduler invokes through its role")
	}
}

func TestRankerSchedule_ExampleEnvironmentPassesIt(t *testing.T) {
	example := loadTerraform(t, filepath.Join("environments", "example"))
	var main string
	for _, m := range blocks(example, "module", "santa_api") {
		main = string(m.Body.SrcRange.SliceBytes(srcOf(t, example, m)))
	}
	raw, err := os.ReadFile(filepath.Join("environments", "example", "config.auto.tfvars.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"ranker_schedule_expression", "ranker_schedule_timezone"} {
		if !strings.Contains(main, v) {
			t.Errorf("the example environment does not pass %s to the module", v)
		}
		if len(blocks(example, "variable", v)) != 1 {
			t.Errorf("the example environment does not declare variable %s", v)
		}
		if _, ok := cfg[v]; !ok {
			t.Errorf("config.auto.tfvars.json has no %q", v)
		}
	}
}
