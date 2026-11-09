package envcheck

// Found in the first production deployment: make init-env copies the example environment's main.tf and _variables.tf
// once, so a later release that adds a module variable leaves an existing environment without it. Terraform only warns
// about the undeclared value in config.auto.tfvars.json, and the plan goes ahead without the setting. Check finds such an
// environment before plan or apply runs.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const exampleVariables = `
variable "prefix" {
  type = string
}

variable "region" {
  type = string
}

variable "display_time_zone" {
  type    = string
  default = "UTC"
}
`

const exampleMain = `
module "app" {
  source = "../../terraform_modules/app"

  prefix            = var.prefix
  region            = var.region
  display_time_zone = var.display_time_zone
}
`

const exampleConfig = `{
  "prefix": "example",
  "region": "us-east-1",
  "display_time_zone": "UTC"
}`

func writeEnv(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// dirs returns an example environment and an environment directory, each written from its own files.
func dirs(t *testing.T, env map[string]string) (envDir, exampleDir string) {
	t.Helper()
	root := t.TempDir()
	exampleDir = filepath.Join(root, "example")
	envDir = filepath.Join(root, "prod")
	writeEnv(t, exampleDir, map[string]string{
		"main.tf":                 exampleMain,
		"_variables.tf":           exampleVariables,
		"config.auto.tfvars.json": exampleConfig,
	})
	writeEnv(t, envDir, env)
	return envDir, exampleDir
}

func check(t *testing.T, envDir, exampleDir string) []string {
	t.Helper()
	problems, err := Check(envDir, exampleDir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return problems
}

func requireProblem(t *testing.T, problems []string, wants ...string) {
	t.Helper()
	for _, p := range problems {
		ok := true
		for _, w := range wants {
			if !strings.Contains(p, w) {
				ok = false
			}
		}
		if ok {
			return
		}
	}
	t.Errorf("no problem mentions all of %q; got:\n%s", wants, strings.Join(problems, "\n"))
}

func TestCheck_CopyOfExamplePasses(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{
		"main.tf":                 exampleMain,
		"_variables.tf":           exampleVariables,
		"config.auto.tfvars.json": `{"prefix": "prod", "region": "us-west-2", "display_time_zone": "Asia/Tokyo"}`,
	})
	if problems := check(t, envDir, exampleDir); len(problems) != 0 {
		t.Errorf("an environment copied from the example has problems:\n%s", strings.Join(problems, "\n"))
	}
}

// The case found in production: the environment predates display_time_zone, and its config sets it anyway.
func TestCheck_EnvironmentFromAnOlderRelease(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{
		"main.tf": `
module "app" {
  source = "../../terraform_modules/app"

  prefix = var.prefix
  region = var.region
}
`,
		"_variables.tf": `
variable "prefix" {
  type = string
}

variable "region" {
  type = string
}
`,
		"config.auto.tfvars.json": `{"prefix": "prod", "region": "us-west-2", "display_time_zone": "Asia/Tokyo"}`,
	})
	problems := check(t, envDir, exampleDir)
	requireProblem(t, problems, "display_time_zone", "_variables.tf")
	requireProblem(t, problems, "display_time_zone", "main.tf")
	requireProblem(t, problems, "display_time_zone", "config.auto.tfvars.json")
}

// An environment that has not set the new value yet still misses the declaration and the module argument: without
// them, setting it later would be ignored the same way.
func TestCheck_MissingVariableWithoutAConfigValue(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{
		"main.tf": `
module "app" {
  source = "../../terraform_modules/app"

  prefix = var.prefix
  region = var.region
}
`,
		"_variables.tf": `
variable "prefix" {
  type = string
}

variable "region" {
  type = string
}
`,
		"config.auto.tfvars.json": `{"prefix": "prod", "region": "us-west-2"}`,
	})
	problems := check(t, envDir, exampleDir)
	requireProblem(t, problems, "display_time_zone", "_variables.tf")
	requireProblem(t, problems, "display_time_zone", "main.tf")
	for _, p := range problems {
		if strings.Contains(p, "config.auto.tfvars.json") {
			t.Errorf("config.auto.tfvars.json sets nothing undeclared, but: %s", p)
		}
	}
}

// Declared but not passed to the module: Terraform accepts it silently and the module default applies.
func TestCheck_VariableDeclaredButNotPassedToTheModule(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{
		"main.tf": `
module "app" {
  source = "../../terraform_modules/app"

  prefix = var.prefix
  region = var.region
}
`,
		"_variables.tf":           exampleVariables,
		"config.auto.tfvars.json": `{"prefix": "prod", "region": "us-west-2", "display_time_zone": "Asia/Tokyo"}`,
	})
	problems := check(t, envDir, exampleDir)
	requireProblem(t, problems, "display_time_zone", "main.tf")
	if len(problems) != 1 {
		t.Errorf("want only the main.tf problem, got:\n%s", strings.Join(problems, "\n"))
	}
}

// The deployment docs tell operators to add module variables the example does not pass, so an environment may have
// more than the example, and variables may live in any .tf file.
func TestCheck_CustomisedEnvironmentPasses(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{
		"main.tf": `
module "app" {
  source = "../../terraform_modules/app"

  prefix            = var.prefix
  region            = var.region
  display_time_zone = var.display_time_zone
  lambda_timeout    = var.lambda_timeout
}
`,
		"_variables.tf": `
variable "prefix" {
  type = string
}

variable "region" {
  type = string
}
`,
		"extra.tf": `
variable "display_time_zone" {
  type = string
}

variable "lambda_timeout" {
  type = number
}
`,
		"config.auto.tfvars.json": `{"prefix": "prod", "region": "us-west-2", "display_time_zone": "Asia/Tokyo", "lambda_timeout": 30}`,
	})
	if problems := check(t, envDir, exampleDir); len(problems) != 0 {
		t.Errorf("a customised environment that has everything the example has was refused:\n%s", strings.Join(problems, "\n"))
	}
}

func TestCheck_UndeclaredValueInOtherAutoTfvars(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{
		"main.tf":                  exampleMain,
		"_variables.tf":            exampleVariables,
		"config.auto.tfvars.json":  `{"prefix": "prod", "region": "us-west-2"}`,
		"private.auto.tfvars.json": `{"display_time_zone": "Asia/Tokyo", "dispaly_time_zone": "Asia/Tokyo"}`,
	})
	problems := check(t, envDir, exampleDir)
	requireProblem(t, problems, "dispaly_time_zone", "private.auto.tfvars.json")
	if len(problems) != 1 {
		t.Errorf("want only the misspelt key, got:\n%s", strings.Join(problems, "\n"))
	}
}

func TestCheck_MissingModuleBlock(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{
		"main.tf":                 `# emptied by mistake`,
		"_variables.tf":           exampleVariables,
		"config.auto.tfvars.json": `{"prefix": "prod", "region": "us-west-2"}`,
	})
	requireProblem(t, check(t, envDir, exampleDir), `module "app"`)
}

func TestCheck_ProblemsAreSortedAndStable(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{
		"main.tf":                 `module "app" {}`,
		"_variables.tf":           ``,
		"config.auto.tfvars.json": `{"prefix": "prod", "region": "us-west-2", "display_time_zone": "UTC"}`,
	})
	first := check(t, envDir, exampleDir)
	for i := 0; i < 5; i++ {
		again := check(t, envDir, exampleDir)
		if strings.Join(again, "\n") != strings.Join(first, "\n") {
			t.Fatalf("Check's output changes between runs:\n%s\n---\n%s", strings.Join(first, "\n"), strings.Join(again, "\n"))
		}
	}
	if len(first) != 9 {
		t.Errorf("want 9 problems (3 declarations, 3 module arguments, 3 config values), got %d:\n%s", len(first), strings.Join(first, "\n"))
	}
}

func TestCheck_Errors(t *testing.T) {
	envDir, exampleDir := dirs(t, map[string]string{"main.tf": exampleMain, "_variables.tf": exampleVariables})
	if _, err := Check(filepath.Join(filepath.Dir(envDir), "missing"), exampleDir); err == nil {
		t.Error("Check accepted an environment directory that does not exist")
	}
	writeEnv(t, envDir, map[string]string{"broken.tf": `variable "x" {`})
	if _, err := Check(envDir, exampleDir); err == nil {
		t.Error("Check accepted a .tf file that does not parse")
	}
	writeEnv(t, envDir, map[string]string{"broken.tf": ``, "config.auto.tfvars.json": `{"prefix": `})
	if _, err := Check(envDir, exampleDir); err == nil {
		t.Error("Check accepted a config.auto.tfvars.json that does not parse")
	}
}

// The real example environment, checked against itself, parses and passes.
func TestCheck_RealExampleEnvironment(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	example := filepath.Join(filepath.Dir(file), "..", "..", "deployments", "environments", "example")
	if problems := check(t, example, example); len(problems) != 0 {
		t.Errorf("the example environment fails its own check:\n%s", strings.Join(problems, "\n"))
	}
}
