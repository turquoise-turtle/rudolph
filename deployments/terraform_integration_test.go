//go:build integration

package deployments

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// D1/D2 end to end: the example environment must `terraform init` and `terraform validate` against the
// modules in this checkout. Runs on a copy so no .terraform state is left in the repository.
func TestD1_ExampleEnvironmentInitsAndValidates(t *testing.T) {
	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Fatal("terraform is required for integration tests")
	}
	tmp := t.TempDir()
	for _, dir := range []string{"environments/example", "terraform_modules"} {
		if out, err := exec.Command("cp", "-R", dir, filepath.Join(tmp, filepath.Base(dir))).CombinedOutput(); err != nil {
			t.Fatalf("copy %s: %v %s", dir, err, out)
		}
	}
	// Keep the same relative layout: environments/<env> -> ../../terraform_modules
	envDir := filepath.Join(tmp, "environments", "example")
	if err := os.MkdirAll(filepath.Dir(envDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(tmp, "example"), envDir); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) {
		cmd := exec.Command(tf, args...)
		cmd.Dir = envDir
		cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_PLUGIN_CACHE_DIR="+pluginCache(t))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("terraform %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-backend=false", "-input=false", "-no-color")
	run("validate", "-no-color")
}

func pluginCache(t *testing.T) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return t.TempDir()
	}
	cache := filepath.Join(dir, "rudolph-terraform-plugins")
	_ = os.MkdirAll(cache, 0o755)
	return cache
}
