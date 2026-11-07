package depcheck

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// No legacy dependencies before the first production deployment (owner, 2026-11-07): the module targets the current
// Go release and requires no deprecated module.
func TestGoDirectiveIsCurrent(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mod), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "go" {
			parts := strings.Split(f[1], ".")
			minor, _ := strconv.Atoi(parts[1])
			if parts[0] != "1" || minor < 27 {
				t.Errorf("go directive %s; want 1.27 or later (the current Go release)", f[1])
			}
			return
		}
	}
	t.Error("go.mod has no go directive")
}

func TestNoDeprecatedModules(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, deprecated := range []string{"github.com/golang/protobuf ", "github.com/aws/aws-sdk-go "} {
		if strings.Contains(string(mod), deprecated) {
			t.Errorf("go.mod requires the deprecated module %s", strings.TrimSpace(deprecated))
		}
	}
}
