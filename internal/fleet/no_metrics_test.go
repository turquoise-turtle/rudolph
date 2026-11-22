package fleet_test

// No custom metrics and no notifications from the fleet job (owner decision, cost round, 2026-11-22): the job publishes
// nothing to CloudWatch (no PutMetricData, no embedded metric format log lines) and sends nothing to SNS. Its only
// alarm is built from Lambda's own Invocations and Errors (deployments/fleet_test.go).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleet_PublishesNoMetricsOrNotifications(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "./internal/fleet", "./cmd/fleet")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./internal/fleet ./cmd/fleet: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, banned := range []string{"/service/cloudwatch", "/service/sns", "/service/eventbridge"} {
			if strings.Contains(dep, banned) {
				t.Errorf("the fleet job depends on %s; it publishes no metrics and sends no notifications", dep)
			}
		}
	}
	for _, dir := range []string{filepath.Join(root, "internal", "fleet"), filepath.Join(root, "cmd", "fleet")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no Go files in %s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, emf := range []string{`"_aws"`, "CloudWatchMetrics", "PutMetricData"} {
				if strings.Contains(string(src), emf) {
					t.Errorf("%s writes CloudWatch metrics (%s)", f, emf)
				}
			}
		}
	}
}
