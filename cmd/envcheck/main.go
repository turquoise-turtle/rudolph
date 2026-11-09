// envcheck checks a deployment environment directory against the example environment before make plan or make deploy
// runs Terraform in it. See internal/envcheck.
//
// Usage: envcheck <envDir> [exampleDir]
//
// exampleDir defaults to the example directory next to envDir. It prints nothing and exits 0 when the environment has
// everything the example has, lists the problems and exits 1 when it does not, and exits 2 on any other error.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/airbnb/rudolph/internal/envcheck"
)

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: envcheck <envDir> [exampleDir]")
		os.Exit(2)
	}
	envDir := os.Args[1]
	exampleDir := filepath.Join(filepath.Dir(filepath.Clean(envDir)), "example")
	if len(os.Args) == 3 {
		exampleDir = os.Args[2]
	}

	problems, err := envcheck.Check(envDir, exampleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "envcheck: %v\n", err)
		os.Exit(2)
	}
	if len(problems) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "The environment %s does not match the example environment %s:\n\n", short(envDir), short(exampleDir))
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "  - %s\n", p)
	}
	fmt.Fprintf(os.Stderr, `
The environment was probably created by make init-env from an older release, and a newer release has added to the
example since. Terraform would plan without the missing settings and only warn about undeclared values.

Compare the environment with the example:

  diff -u %[1]s %[2]s
  diff -u %[3]s %[4]s

If the environment has no customisations of its own, copy the example's main.tf and _variables.tf over it. Otherwise
add what is missing by hand. Then run the command again.
`,
		short(filepath.Join(exampleDir, "main.tf")), short(filepath.Join(envDir, "main.tf")),
		short(filepath.Join(exampleDir, "_variables.tf")), short(filepath.Join(envDir, "_variables.tf")))
	os.Exit(1)
}

// short makes a path relative to the working directory where it can, because the Makefile passes absolute paths and
// the diff commands read more easily from the repository root.
func short(path string) string {
	wd, err := os.Getwd()
	if err != nil {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
