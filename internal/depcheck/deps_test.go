// Package depcheck holds checks on the module's dependencies.
package depcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The AWS SDK for Go v1 reached end of support on 31 July 2025. Nothing in the module, tests included, may use it
// (owner, 2026-11-07); everything uses the v2 SDK.
func TestNoAWSSDKv1(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "-test", "-tags", "integration", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var v1 []string
	for _, p := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(p, "github.com/aws/aws-sdk-go/") || p == "github.com/aws/aws-sdk-go" {
			v1 = append(v1, p)
		}
	}
	if len(v1) > 0 {
		t.Errorf("packages of the AWS SDK for Go v1 are still built: %s", strings.Join(v1, ", "))
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mod), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "github.com/aws/aws-sdk-go" || len(f) >= 3 && f[0] == "require" && f[1] == "github.com/aws/aws-sdk-go" {
			t.Errorf("go.mod still requires the v1 SDK: %s", strings.TrimSpace(line))
		}
	}
}
