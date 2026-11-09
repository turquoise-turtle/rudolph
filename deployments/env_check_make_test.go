package deployments

// make plan and make deploy check the environment directory against the example environment first (see
// internal/envcheck): an environment copied before a release that added a variable otherwise deploys without it.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// makeRule returns the prerequisites and recipe lines of a Makefile rule.
func makeRule(t *testing.T, makefile, target string) (prereqs []string, recipe []string) {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	head := regexp.MustCompile(`^` + regexp.QuoteMeta(target) + `\s*:([^=].*)?$`)
	for i, line := range lines {
		m := head.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		prereqs = strings.Fields(m[1])
		for _, r := range lines[i+1:] {
			if !strings.HasPrefix(r, "\t") {
				break
			}
			recipe = append(recipe, strings.TrimSpace(r))
		}
		return prereqs, recipe
	}
	t.Fatalf("Makefile has no rule %q", target)
	return nil, nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestMake_PlanAndApplyCheckTheEnvironmentFirst(t *testing.T) {
	src, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(src)

	prereqs, recipe := makeRule(t, makefile, ".check-env")
	if !contains(prereqs, ".check-args") {
		t.Errorf(".check-env needs ENV: want .check-args among its prerequisites, got %q", prereqs)
	}
	joined := strings.Join(recipe, "\n")
	if !strings.Contains(joined, "go run ./cmd/envcheck") || !strings.Contains(joined, "$(TERRAFORM_DEPLOYMENTS_ENV_DIR)") {
		t.Errorf(".check-env should run go run ./cmd/envcheck on $(TERRAFORM_DEPLOYMENTS_ENV_DIR), got:\n%s", joined)
	}

	for _, target := range []string{"plan", "justdeploy", "force-api-redeploy"} {
		prereqs, _ := makeRule(t, makefile, target)
		if !contains(prereqs, ".check-env") {
			t.Errorf("make %s does not check the environment first: prerequisites %q", target, prereqs)
		}
	}
	// make deploy reaches justdeploy, which checks; the check must come before the slow test and build steps too.
	prereqs, _ = makeRule(t, makefile, "deploy")
	if len(prereqs) < 2 || prereqs[0] != ".check-args" || prereqs[1] != ".check-env" {
		t.Errorf("make deploy should check the environment right after ENV, before testing and building: got %q", prereqs)
	}
	if !regexp.MustCompile(`(?m)^\.PHONY:.*\s\.check-env(\s|$)`).MatchString(makefile) {
		t.Error(".check-env is not listed in .PHONY")
	}
}
