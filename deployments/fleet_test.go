package deployments

// The fleet job (fleet health and Lockdown readiness, owner approved 2026-11-22, with the cost round's decisions): a
// scheduled Lambda `<prefix>_rudolph_fleet` (cmd/fleet, internal/fleet), built and deployed like the ranker.
//
//   - Function: aws_lambda_function.fleet, arm64, 256 MB, 900 s, its own log group (aws_cloudwatch_log_group.fleet,
//     log_retention_in_days), environment EVENTS_DYNAMODB_NAME, DYNAMODB_NAME (local.dynamodb_table_name) and REGION;
//     no retries (aws_lambda_function_event_invoke_config.fleet). The zip is var.lambda_fleet_zip, which the Makefile
//     passes and scripts/build.sh builds from cmd/fleet; the example environment passes it on.
//   - Role aws_iam_role.fleet (path /rudolph/), least privilege, every dynamodb:LeadingKeys condition paired with its
//     Null check, no Scan, no KMS, CloudWatch or SNS statement, no managed policy:
//       store index DataType_MachineID  Query                      SensorData
//       store table                     Query                      Machine#*, GlobalRules
//       store table                     GetItem                    GroupRegistry, GlobalConfig, FleetHealth
//       events table                    Query                      UnknownSeen#*
//       events table                    PutItem, BatchWriteItem    FleetSnapshot#*, FleetPointer
//     and logs:CreateLogStream and logs:PutLogEvents on its own log group only. Its ARN is passed to the store module
//     so a narrowed store key keeps allowing it (data_exposure_test.go's tableRoles).
//   - Schedule: an EventBridge Scheduler schedule on var.fleet_schedule_expression (default once a day, "rate(1 day)",
//     validated like the ranker's) in var.fleet_schedule_timezone (default "UTC"), no retries, through a scheduler
//     role that only this schedule may assume (aws:SourceArn) and that may only invoke the fleet function.
//   - One alarm, aws_cloudwatch_metric_alarm.fleet_job: "no successful run for N days", N = var.fleet_alarm_days
//     (default 3, validated 1 to 7: CloudWatch evaluates at most 7 days of daily periods). Metric math on the function's
//     own AWS/Lambda Invocations minus Errors, Sum over 86,400 s, below 1 for N periods in a row, missing data treated
//     as breaching, notifying var.alarm_sns_topic_arn on alarm and recovery, and created only when that topic is set.
//     No other alarm, metric filter or custom metric for the job: it is left out of lambda_errors and of the
//     lambda_timeouts log filters (a timeout is an Error, so the one alarm sees it).

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

func santaAPIModule(t *testing.T) []tfFile {
	t.Helper()
	return filesIn(loadTerraform(t, santaAPIDir), santaAPIDir)
}

func oneBlock(t *testing.T, files []tfFile, typ string, labels ...string) *hclsyntax.Block {
	t.Helper()
	bs := blocks(files, typ, labels...)
	if len(bs) != 1 {
		t.Fatalf("found %d %s %v, want 1", len(bs), typ, labels)
	}
	return bs[0]
}

func attrSource(t *testing.T, files []tfFile, b *hclsyntax.Block, name string) string {
	t.Helper()
	a, ok := b.Body.Attributes[name]
	if !ok {
		return ""
	}
	return strings.TrimSpace(sourceOf(t, files, a.Expr))
}

func TestFleetFunction_BuiltDeployedAndConfigured(t *testing.T) {
	module := santaAPIModule(t)
	fn := oneBlock(t, module, "resource", "aws_lambda_function", "fleet")
	if src := attrSource(t, module, fn, "function_name"); !strings.Contains(inlineLocals(t, module, src), "_rudolph_fleet") {
		t.Errorf("function_name %s, want ${var.prefix}_rudolph_fleet", src)
	}
	for attr, want := range map[string]string{"memory_size": "256", "timeout": "900", "role": "aws_iam_role.fleet.arn", "architectures": `["arm64"]`} {
		if got := squash(attrSource(t, module, fn, attr)); got != want {
			t.Errorf("fleet function %s = %q, want %q", attr, got, want)
		}
	}
	env := nested(fn, "environment")
	if len(env) != 1 {
		t.Fatal("the fleet function has no environment block")
	}
	vars := attrSource(t, module, env[0], "variables")
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`EVENTS_DYNAMODB_NAME\s*=\s*module\.rule_store\.events_dynamodb_table_name`),
		regexp.MustCompile(`DYNAMODB_NAME\s*=\s*local\.dynamodb_table_name`),
		regexp.MustCompile(`REGION\s*=\s*var\.region`),
	} {
		if !want.MatchString(vars) {
			t.Errorf("fleet environment does not set %s:\n%s", want, vars)
		}
	}
	for _, unwanted := range []string{"FLEET_FIRST_SEEN_FLOOR", "FLEET_STALE_AFTER"} {
		if strings.Contains(vars, unwanted) {
			t.Errorf("fleet environment sets %s: there is no backfill floor, and the stale threshold is FleetHealth / Config", unwanted)
		}
	}
	if !strings.Contains(attrSource(t, module, fn, "depends_on"), "aws_cloudwatch_log_group.fleet") {
		t.Error("the fleet function does not depend on its log group (Lambda would create one without retention)")
	}
	lg := oneBlock(t, module, "resource", "aws_cloudwatch_log_group", "fleet")
	if got := attrSource(t, module, lg, "retention_in_days"); got != "var.log_retention_in_days" {
		t.Errorf("fleet log group retention %q, want var.log_retention_in_days", got)
	}
	invoke := oneBlock(t, module, "resource", "aws_lambda_function_event_invoke_config", "fleet")
	if got := attrSource(t, module, invoke, "maximum_retry_attempts"); got != "0" {
		t.Errorf("fleet maximum_retry_attempts %q, want 0: the next day's run rebuilds everything", got)
	}
	if len(blocks(module, "variable", "lambda_fleet_zip")) != 1 {
		t.Error("santa_api declares no variable lambda_fleet_zip")
	}

	makefile, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`--var lambda_fleet_zip=`).Match(makefile) {
		t.Error("the Makefile does not pass --var lambda_fleet_zip to Terraform")
	}
	build, err := os.ReadFile(filepath.Join("..", "scripts", "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`GOARCH=arm64 go build [^\n]*\$APPS_DIR/fleet\b`).Match(build) {
		t.Error("scripts/build.sh does not build cmd/fleet for linux/arm64")
	}
	if !strings.Contains(string(build), "fleet_deployment.zip") {
		t.Error("scripts/build.sh does not package fleet_deployment.zip")
	}

	example := loadTerraform(t, filepath.Join("environments", "example"))
	main := ""
	for _, m := range blocks(example, "module", "santa_api") {
		main = string(m.Body.SrcRange.SliceBytes(srcOf(t, example, m)))
	}
	for _, v := range []string{"lambda_fleet_zip", "fleet_schedule_expression", "fleet_schedule_timezone", "fleet_alarm_days"} {
		if !strings.Contains(main, v) {
			t.Errorf("the example environment does not pass %s to the module", v)
		}
		if len(blocks(example, "variable", v)) != 1 {
			t.Errorf("the example environment does not declare variable %s", v)
		}
	}
}

// fleetStatement is one statement of the fleet role's DynamoDB policy, reduced to what least privilege is about.
type fleetStatement struct {
	resource string // "store index DataType_MachineID", "store table" or "events table"
	actions  string // sorted, comma separated
	keys     string // sorted leading key values, comma separated
}

func (s fleetStatement) String() string { return s.resource + ": " + s.actions + " on " + s.keys }

func TestFleetIAM_LeastPrivilege(t *testing.T) {
	module := santaAPIModule(t)
	role := oneBlock(t, module, "resource", "aws_iam_role", "fleet")
	if got := attrSource(t, module, role, "path"); got != `"/rudolph/"` {
		t.Errorf("fleet role path %s, want \"/rudolph/\"", got)
	}

	// Every policy attached to the role, and no managed policy.
	var docs []*hclsyntax.Block
	for _, rp := range blocks(module, "resource", "aws_iam_role_policy") {
		if !strings.Contains(attrSource(t, module, rp, "role"), "aws_iam_role.fleet.") {
			continue
		}
		pol := attrSource(t, module, rp, "policy")
		for _, d := range blocks(module, "data", "aws_iam_policy_document") {
			if strings.Contains(pol, "data.aws_iam_policy_document."+d.Labels[1]+".") {
				docs = append(docs, d)
			}
		}
	}
	for _, a := range blocks(module, "resource", "aws_iam_role_policy_attachment") {
		if strings.Contains(attrSource(t, module, a, "role"), "aws_iam_role.fleet.") {
			t.Errorf("a managed policy is attached to the fleet role (%s)", a.Labels[1])
		}
	}
	if len(docs) == 0 {
		t.Fatal("no policy document is attached to aws_iam_role.fleet")
	}

	var got []string
	logs := false
	for _, doc := range docs {
		for _, st := range statementsOf(doc) {
			text := squash(blockText(t, module, st))
			actions := regexp.MustCompile(`"([a-z0-9]+:[A-Za-z*]+)"`).FindAllStringSubmatch(attrSource(t, module, st, "actions"), -1)
			var names []string
			for _, a := range actions {
				names = append(names, a[1])
			}
			sort.Strings(names)
			resources := attrSource(t, module, st, "resources")
			if strings.Contains(resources, `"*"`) {
				t.Errorf("a fleet statement has a wildcard resource: %s", text)
			}
			if strings.HasPrefix(strings.Join(names, ","), "logs:") {
				if strings.Join(names, ",") != "logs:CreateLogStream,logs:PutLogEvents" || !strings.Contains(resources, "aws_cloudwatch_log_group.fleet.arn") {
					t.Errorf("fleet logs statement %v on %s, want CreateLogStream and PutLogEvents on its own log group", names, resources)
				}
				logs = true
				continue
			}
			for _, name := range names {
				if !strings.HasPrefix(name, "dynamodb:") {
					t.Errorf("the fleet role may %s; it needs DynamoDB and its own logs only (no metrics, no notifications, no KMS)", name)
				}
				if name == "dynamodb:Scan" || strings.Contains(name, "*") {
					t.Errorf("the fleet role may %s", name)
				}
			}
			var resource string
			switch {
			case strings.Contains(resources, "events_dynamodb_table_arn") && !strings.Contains(resources, "/index/"):
				resource = "events table"
			case strings.Contains(resources, "/index/DataType_MachineID") && !strings.Contains(resources, "events"):
				resource = "store index DataType_MachineID"
			case strings.Contains(resources, "dynamodb_table_arn") && !strings.Contains(resources, "/index/") && !strings.Contains(resources, "events"):
				resource = "store table"
			default:
				t.Errorf("fleet statement on unexpected resources %s", resources)
				continue
			}
			var keys []string
			var forAll, null bool
			for _, cond := range nested(st, "condition") {
				variable, _ := literal(cond.Body.Attributes["variable"])
				test, _ := literal(cond.Body.Attributes["test"])
				if variable != "dynamodb:LeadingKeys" {
					t.Errorf("fleet statement has a condition on %s", variable)
					continue
				}
				if test == "Null" {
					null = strings.Contains(squash(blockText(t, module, cond)), `"false"`)
					continue
				}
				if !strings.HasPrefix(test, "ForAllValues:") {
					t.Errorf("LeadingKeys condition test %q, want ForAllValues:StringEquals or StringLike", test)
				}
				forAll = true
				val, diags := cond.Body.Attributes["values"].Expr.Value(nil)
				if diags.HasErrors() {
					t.Fatalf("LeadingKeys values are not literal: %s", diags.Error())
				}
				for it := val.ElementIterator(); it.Next(); {
					_, e := it.Element()
					keys = append(keys, e.AsString())
				}
			}
			if !forAll || !null {
				t.Errorf("fleet statement %s: LeadingKeys condition %v, Null check %v; want both", text, forAll, null)
			}
			sort.Strings(keys)
			got = append(got, fleetStatement{resource, strings.Join(names, ","), strings.Join(keys, ",")}.String())
		}
	}
	want := []string{
		fleetStatement{"events table", "dynamodb:BatchWriteItem,dynamodb:PutItem", "FleetPointer,FleetSnapshot#*"}.String(),
		fleetStatement{"events table", "dynamodb:Query", "UnknownSeen#*"}.String(),
		fleetStatement{"store index DataType_MachineID", "dynamodb:Query", "SensorData"}.String(),
		fleetStatement{"store table", "dynamodb:GetItem", "FleetHealth,GlobalConfig,GroupRegistry"}.String(),
		fleetStatement{"store table", "dynamodb:Query", "GlobalRules,Machine#*"}.String(),
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("fleet role statements:\n  %s\nwant exactly:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if !logs {
		t.Error("the fleet role has no statement for its own log group")
	}
}

// The ranker's role is unchanged by the fleet job.
func TestFleetIAM_RankerRoleUnchanged(t *testing.T) {
	module := santaAPIModule(t)
	doc := oneBlock(t, module, "data", "aws_iam_policy_document", "ranker")
	text := blockText(t, module, doc)
	for _, k := range []string{"UnknownSeen", "FleetSnapshot", "FleetPointer", "FleetHealth", "Machine#"} {
		if strings.Contains(text, k) {
			t.Errorf("the ranker's policy names %s; the fleet job has its own role", k)
		}
	}
}

func TestFleetSchedule_DailyVariableAndNoRetries(t *testing.T) {
	module := santaAPIModule(t)
	for name, want := range map[string]string{"fleet_schedule_expression": `"rate(1 day)"`, "fleet_schedule_timezone": `"UTC"`} {
		if d, ok := variableDefault(t, module, name); !ok || d != want {
			t.Errorf("variable %s default %s, want %s", name, d, want)
		}
	}
	v := oneBlock(t, module, "variable", "fleet_schedule_expression")
	validations := nested(v, "validation")
	if len(validations) != 1 || !strings.Contains(attrSource(t, module, validations[0], "condition"), "rate|cron|at") {
		t.Error("fleet_schedule_expression is not validated as a rate(), cron() or at() expression, like the ranker's")
	}

	var schedule *hclsyntax.Block
	for _, s := range blocks(module, "resource", "aws_scheduler_schedule") {
		for _, target := range nested(s, "target") {
			if strings.Contains(attrSource(t, module, target, "arn"), "aws_lambda_function.fleet.") {
				schedule = s
			}
		}
	}
	if schedule == nil {
		t.Fatal("no aws_scheduler_schedule targets aws_lambda_function.fleet")
	}
	for attr, want := range map[string]string{"schedule_expression": "var.fleet_schedule_expression", "schedule_expression_timezone": "var.fleet_schedule_timezone"} {
		if got := attrSource(t, module, schedule, attr); got != want {
			t.Errorf("fleet schedule %s = %q, want %s", attr, got, want)
		}
	}
	if name := attrSource(t, module, schedule, "name"); !strings.Contains(name, "var.prefix") || !strings.Contains(name, "_rudolph_fleet") {
		t.Errorf("fleet schedule name %s, want ${var.prefix}_rudolph_fleet", name)
	}
	target := nested(schedule, "target")[0]
	retries := nested(target, "retry_policy")
	if len(retries) != 1 || attrSource(t, module, retries[0], "maximum_retry_attempts") != "0" {
		t.Error("the fleet schedule retries; want retry_policy { maximum_retry_attempts = 0 }")
	}
	roleSrc := attrSource(t, module, target, "role_arn")
	if roleSrc != "aws_iam_role.fleet_scheduler.arn" {
		t.Fatalf("fleet schedule role_arn %s, want aws_iam_role.fleet_scheduler.arn (a role of its own)", roleSrc)
	}

	docs, files := policyDocs(t, santaAPI)
	trust := docs["fleet_scheduler_assume_role"]
	if trust == nil {
		t.Fatal("no fleet_scheduler_assume_role policy document")
	}
	trustText := blockText(t, files, trust)
	for _, want := range []string{"scheduler.amazonaws.com", "aws:SourceArn", "aws:SourceAccount", "_rudolph_fleet"} {
		if !strings.Contains(trustText, want) {
			t.Errorf("the fleet scheduler role's trust policy lacks %s", want)
		}
	}
	invoke := docs["fleet_scheduler"]
	if invoke == nil {
		t.Fatal("no fleet_scheduler policy document")
	}
	invokeText := squash(blockText(t, files, invoke))
	if !strings.Contains(invokeText, `"lambda:InvokeFunction"`) || !strings.Contains(invokeText, "aws_lambda_function.fleet.arn") || strings.Contains(invokeText, `"*"`) {
		t.Errorf("the fleet scheduler role may do more than invoke the fleet function:\n%s", invokeText)
	}
	if strings.Count(invokeText, "aws_lambda_function.") != 1 {
		t.Errorf("the fleet scheduler role may invoke another function:\n%s", invokeText)
	}
}

// fleetAlarmDaysAccepts evaluates the validation of var.fleet_alarm_days for n.
func fleetAlarmDaysAccepts(t *testing.T, files []tfFile, v *hclsyntax.Block, n int) bool {
	t.Helper()
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{"var": cty.ObjectVal(map[string]cty.Value{"fleet_alarm_days": cty.NumberIntVal(int64(n))})},
		Functions: map[string]function.Function{"floor": stdlib.FloorFunc, "ceil": stdlib.CeilFunc, "abs": stdlib.AbsoluteFunc, "min": stdlib.MinFunc, "max": stdlib.MaxFunc},
	}
	ok := true
	for _, val := range nested(v, "validation") {
		cond, diags := val.Body.Attributes["condition"].Expr.Value(ctx)
		if diags.HasErrors() {
			t.Fatalf("fleet_alarm_days validation does not evaluate: %s", diags.Error())
		}
		ok = ok && cond.True()
	}
	return ok
}

func TestFleetAlarm_NoSuccessfulRunForNDays(t *testing.T) {
	module := santaAPIModule(t)
	if d, ok := variableDefault(t, module, "fleet_alarm_days"); !ok || d != "3" {
		t.Errorf("variable fleet_alarm_days default %s, want 3", d)
	}
	v := oneBlock(t, module, "variable", "fleet_alarm_days")
	if len(nested(v, "validation")) == 0 {
		t.Fatal("fleet_alarm_days has no validation")
	}
	for n, want := range map[int]bool{0: false, 1: true, 3: true, 7: true, 8: false} {
		if got := fleetAlarmDaysAccepts(t, module, v, n); got != want {
			t.Errorf("fleet_alarm_days = %d: accepted %v, want %v (1 to 7)", n, got, want)
		}
	}

	alarm := oneBlock(t, module, "resource", "aws_cloudwatch_metric_alarm", "fleet_job")
	if got := attrSource(t, module, alarm, "alarm_name"); !strings.Contains(got, "var.prefix") || !strings.Contains(got, "_rudolph_fleet_job") {
		t.Errorf("alarm_name %s, want ${var.prefix}_rudolph_fleet_job", got)
	}
	if count := attrSource(t, module, alarm, "count"); !strings.Contains(inlineLocals(t, module, count), "var.alarm_sns_topic_arn") {
		t.Errorf("the fleet alarm's count is %q; it must exist only when alarm_sns_topic_arn is set", count)
	}
	for attr, want := range map[string]string{
		"evaluation_periods":  "var.fleet_alarm_days",
		"threshold":           "1",
		"comparison_operator": `"LessThanThreshold"`,
		"treat_missing_data":  `"breaching"`,
	} {
		if got := attrSource(t, module, alarm, attr); got != want {
			t.Errorf("fleet alarm %s = %s, want %s", attr, got, want)
		}
	}
	if got := attrSource(t, module, alarm, "datapoints_to_alarm"); got != "" && got != "var.fleet_alarm_days" {
		t.Errorf("fleet alarm datapoints_to_alarm = %s, want var.fleet_alarm_days or unset (N days in a row)", got)
	}
	for _, attr := range []string{"alarm_actions", "ok_actions"} {
		if got := inlineLocals(t, module, attrSource(t, module, alarm, attr)); !strings.Contains(got, "alarm_sns_topic_arn") {
			t.Errorf("fleet alarm %s %q does not notify alarm_sns_topic_arn", attr, got)
		}
	}
	for _, attr := range []string{"namespace", "metric_name"} {
		if _, ok := alarm.Body.Attributes[attr]; ok {
			t.Errorf("the fleet alarm sets %s; it is metric math over two metrics", attr)
		}
	}

	// Metric math: Invocations - Errors of the fleet function, daily sums.
	ids := map[string]string{} // metric name -> query id
	var expression string
	for _, q := range nested(alarm, "metric_query") {
		id, _ := literal(q.Body.Attributes["id"])
		if e := attrSource(t, module, q, "expression"); e != "" {
			if attrSource(t, module, q, "return_data") != "true" {
				t.Errorf("the expression query %s does not return data", id)
			}
			expression = e
			continue
		}
		for _, m := range nested(q, "metric") {
			name, _ := literal(m.Body.Attributes["metric_name"])
			ns, _ := literal(m.Body.Attributes["namespace"])
			stat, _ := literal(m.Body.Attributes["stat"])
			if ns != "AWS/Lambda" || stat != "Sum" || attrSource(t, module, m, "period") != "86400" {
				t.Errorf("metric %s: namespace %q, stat %q, period %s; want AWS/Lambda, Sum, 86400", name, ns, stat, attrSource(t, module, m, "period"))
			}
			if dims := attrSource(t, module, m, "dimensions"); !regexp.MustCompile(`FunctionName\s*=\s*aws_lambda_function\.fleet\.function_name`).MatchString(dims) {
				t.Errorf("metric %s is not the fleet function's (dimensions %s)", name, dims)
			}
			ids[name] = id
		}
	}
	if ids["Invocations"] == "" || ids["Errors"] == "" || len(ids) != 2 {
		t.Fatalf("the fleet alarm's metrics are %v, want exactly Invocations and Errors", ids)
	}
	compact := strings.ReplaceAll(strings.Trim(expression, `"`), " ", "")
	if !strings.Contains(compact, ids["Invocations"]+"-"+ids["Errors"]) && !strings.Contains(compact, ids["Invocations"]+",0)-FILL("+ids["Errors"]) {
		t.Errorf("the fleet alarm's expression %s is not %s - %s", expression, ids["Invocations"], ids["Errors"])
	}
}

// One alarm only: nothing else watches the fleet function or its logs, and the job has no custom metric.
func TestFleetAlarm_IsTheOnlyMonitoringOfTheJob(t *testing.T) {
	module := santaAPIModule(t)
	for _, a := range blocks(module, "resource", "aws_cloudwatch_metric_alarm") {
		if a.Labels[1] == "fleet_job" {
			continue
		}
		text := blockText(t, module, a)
		for _, ref := range []string{"aws_lambda_function.fleet", "aws_cloudwatch_log_group.fleet", "Fleet"} {
			if strings.Contains(text, ref) {
				t.Errorf("alarm %s also watches the fleet job (%s)", a.Labels[1], ref)
			}
		}
	}
	for _, f := range blocks(module, "resource", "aws_cloudwatch_log_metric_filter") {
		text := inlineLocals(t, module, blockText(t, module, f))
		if strings.Contains(text, "aws_cloudwatch_log_group.fleet") || strings.Contains(text, "fleet_summary") || strings.Contains(text, "Fleet") {
			t.Errorf("metric filter %s reads the fleet job's logs or makes a fleet metric", f.Labels[1])
		}
	}
	for _, local := range []string{"error_alarm_function_names", "all_log_groups", "handler_log_groups"} {
		for _, l := range blocks(module, "locals") {
			if a, ok := l.Body.Attributes[local]; ok && strings.Contains(sourceOf(t, module, a.Expr), "fleet") {
				t.Errorf("local.%s includes the fleet job; its one alarm covers errors and timeouts", local)
			}
		}
	}
	all := allText(t, santaAPI)
	for _, metric := range []string{"FleetStalePercent", "FleetIncompletePercent", "fleet_stale_alarm_percent", "fleet_incomplete_alarm_percent"} {
		if strings.Contains(all, metric) {
			t.Errorf("the module still has %s; the owner dropped the stale and incomplete alarms", metric)
		}
	}
}

func TestFleetIAM_OwnLogsAndPinnedTrust(t *testing.T) {
	text := allText(t, santaAPI)
	if !strings.Contains(text, "aws_cloudwatch_log_group.fleet.arn") {
		t.Error("no log grant refers to aws_cloudwatch_log_group.fleet.arn")
	}
	docs, files := policyDocs(t, santaAPI)
	trust := docs["fleet_assume_role"]
	if trust == nil {
		t.Fatal("no fleet_assume_role policy document")
	}
	if got := squash(blockText(t, files, trust)); !strings.Contains(got, "lambda.amazonaws.com") || strings.Contains(got, "scheduler.amazonaws.com") {
		t.Errorf("the fleet role should be assumable by Lambda only:\n%s", got)
	}
}
