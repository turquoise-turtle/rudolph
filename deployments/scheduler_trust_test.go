package deployments

// The scheduler roles' trust (upgrade step 31). EventBridge Scheduler sets aws:SourceArn to the schedule GROUP's ARN
// when it assumes an execution role, so a trust condition naming the schedule's own ARN never matches: Scheduler then
// refuses to create the schedule ("must allow AWS EventBridge Scheduler to assume the role") and an existing schedule
// stops invoking its function without any error in the function's log. Each scheduler role must therefore pin
// aws:SourceArn to arn:aws:scheduler:<region>:<account>:schedule-group/default with aws:SourceAccount beside it, and
// the schedules must stay in the default group (no group_name) so that condition matches them.

import (
	"strings"
	"testing"
)

func TestSchedulerTrust_PinsTheScheduleGroupNotTheSchedule(t *testing.T) {
	docs, files := policyDocs(t, santaAPI)
	for _, name := range []string{"ranker_scheduler_assume_role", "fleet_scheduler_assume_role"} {
		doc := docs[name]
		if doc == nil {
			t.Errorf("no %s policy document", name)
			continue
		}
		text := squash(blockText(t, files, doc))
		want := `"arn:aws:scheduler:${var.region}:${var.aws_account_id}:schedule-group/default"`
		if !strings.Contains(text, want) {
			t.Errorf("%s does not pin aws:SourceArn to %s:\n%s", name, want, text)
		}
		if strings.Contains(text, ":schedule/") {
			t.Errorf("%s names a schedule ARN, which Scheduler never presents as aws:SourceArn:\n%s", name, text)
		}
		for _, want := range []string{"aws:SourceAccount", "var.aws_account_id", "scheduler.amazonaws.com", "sts:AssumeRole"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %s", name, want)
			}
		}
	}
}

func TestSchedulerTrust_SchedulesStayInTheDefaultGroup(t *testing.T) {
	module := santaAPIModule(t)
	schedules := blocks(module, "resource", "aws_scheduler_schedule")
	if len(schedules) != 2 {
		t.Fatalf("found %d aws_scheduler_schedule resources, want 2 (ranker and fleet)", len(schedules))
	}
	for _, s := range schedules {
		if g := attrSource(t, module, s, "group_name"); g != "" && g != `"default"` {
			t.Errorf("aws_scheduler_schedule.%s is in group %s; the scheduler roles trust only the default group", s.Labels[1], g)
		}
	}
}
