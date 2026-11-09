// Package envcheck compares a deployment environment directory with the example environment it was created from.
//
// make init-env copies the example's main.tf and _variables.tf once. A later release that adds a module variable leaves
// an existing environment without it, and Terraform only warns about a value for an undeclared variable in an
// .auto.tfvars.json file, so the plan goes ahead without the setting. Operators may customise their environment, so the
// check is that the environment has everything the example has, not that the two are identical.
package envcheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// moduleArgsNotCompared are module arguments that choose where the module comes from rather than configure it, so an
// environment may set them differently (or, for a local module, not set version at all).
var moduleArgsNotCompared = map[string]bool{"source": true, "version": true}

type module struct {
	file string
	args map[string]bool
}

type config struct {
	// variables maps each declared variable to the base name of the file declaring it.
	variables map[string]string
	modules   map[string]module
}

// Check returns one line per way the environment in envDir falls short of the example in exampleDir, sorted. It returns
// an error when either directory cannot be read or a .tf or .tfvars.json file in the environment does not parse.
func Check(envDir, exampleDir string) ([]string, error) {
	env, err := load(envDir)
	if err != nil {
		return nil, fmt.Errorf("environment: %w", err)
	}
	example, err := load(exampleDir)
	if err != nil {
		return nil, fmt.Errorf("example environment: %w", err)
	}

	var problems []string
	for name, file := range example.variables {
		if _, ok := env.variables[name]; !ok {
			problems = append(problems, fmt.Sprintf("variable %q is declared in the example's %s but not in this environment", name, file))
		}
	}
	for label, exMod := range example.modules {
		envMod, ok := env.modules[label]
		if !ok {
			problems = append(problems, fmt.Sprintf("module %q is in the example's %s but not in this environment", label, exMod.file))
			continue
		}
		for arg := range exMod.args {
			if !envMod.args[arg] && !moduleArgsNotCompared[arg] {
				problems = append(problems, fmt.Sprintf("module %q sets %s in the example's %s but not in this environment's %s", label, arg, exMod.file, envMod.file))
			}
		}
	}

	tfvars, err := tfvarsFiles(envDir)
	if err != nil {
		return nil, fmt.Errorf("environment: %w", err)
	}
	for _, path := range tfvars {
		keys, err := jsonKeys(path)
		if err != nil {
			return nil, fmt.Errorf("environment: %w", err)
		}
		for _, key := range keys {
			if _, ok := env.variables[key]; !ok {
				problems = append(problems, fmt.Sprintf("%s sets %q, but this environment declares no such variable, so Terraform would ignore it with only a warning", filepath.Base(path), key))
			}
		}
	}

	sort.Strings(problems)
	return problems, nil
}

// load parses every .tf file directly in dir. Subdirectories, .terraform among them, are not read.
func load(dir string) (*config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cfg := &config{variables: map[string]string{}, modules: map[string]module{}}
	parser := hclparse.NewParser()
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		file, diags := parser.ParseHCL(src, path)
		if diags.HasErrors() {
			return nil, diags
		}
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			return nil, fmt.Errorf("%s: not native HCL syntax", path)
		}
		for _, block := range body.Blocks {
			if len(block.Labels) != 1 {
				continue
			}
			label := block.Labels[0]
			switch block.Type {
			case "variable":
				if _, seen := cfg.variables[label]; !seen {
					cfg.variables[label] = e.Name()
				}
			case "module":
				if _, seen := cfg.modules[label]; seen {
					continue
				}
				args := map[string]bool{}
				for name := range block.Body.Attributes {
					args[name] = true
				}
				cfg.modules[label] = module{file: e.Name(), args: args}
			}
		}
	}
	return cfg, nil
}

// tfvarsFiles returns the JSON variable files Terraform loads automatically from dir, in name order.
func tfvarsFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if name == "terraform.tfvars.json" || strings.HasSuffix(name, ".auto.tfvars.json") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	return files, nil
}

func jsonKeys(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(src, &values); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	return keys, nil
}
