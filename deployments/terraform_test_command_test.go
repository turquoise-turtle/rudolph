//go:build integration

package deployments

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The static tests read count and for_each expressions; terraform test plans the module with a mocked provider for
// each combination of the audit's switches (terraform_modules/santa_api/tests), so a condition that reads right but
// evaluates wrong (inverted, or || another switch) fails too. It needs terraform and the network, so it is an
// integration test (make test-integration), not part of the make test that make deploy runs. Runs on a copy so no
// .terraform is left in the repository.
func TestTFA7_OptionalFeaturesFollowTheirSwitches(t *testing.T) {
	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Fatal("terraform is required for integration tests")
	}
	tests, _ := filepath.Glob(filepath.Join(santaAPI, "tests", "*.tftest.hcl"))
	if len(tests) == 0 {
		t.Fatalf("no %s/tests/*.tftest.hcl", santaAPI)
	}

	module := filepath.Join(t.TempDir(), "santa_api")
	if out, err := exec.Command("cp", "-R", santaAPI, module).CombinedOutput(); err != nil {
		t.Fatalf("copy %s: %v %s", santaAPI, err, out)
	}
	_ = os.RemoveAll(filepath.Join(module, ".terraform"))
	// The example's lock file, when there is one, so the test plans with the provider environments are pinned to.
	if lock, err := os.ReadFile(exampleLock); err == nil {
		if err := os.WriteFile(filepath.Join(module, ".terraform.lock.hcl"), lock, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run := func(args ...string) {
		cmd := exec.Command(tf, args...)
		cmd.Dir = module
		cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_PLUGIN_CACHE_DIR="+pluginCache(t))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("terraform %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-backend=false", "-input=false", "-no-color")
	run("test", "-no-color")
}
