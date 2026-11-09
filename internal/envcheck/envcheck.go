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

	"github.com/hashicorp/hcl/v2"
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

// load parses every .tf file directly in dir, and the module arguments of override.tf.json and *_override.tf.json.
// Subdirectories, .terraform among them, are not read. As in Terraform, override files declare no new variables or
// modules: their module arguments are merged into the base module blocks of the same label.
func load(dir string) (*config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cfg := &config{variables: map[string]string{}, modules: map[string]module{}}
	overrides := map[string]map[string]bool{}
	parser := hclparse.NewParser()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || ignoredFile(name) {
			continue
		}
		var override bool
		switch {
		case strings.HasSuffix(name, ".tf.json"):
			override = isOverride(strings.TrimSuffix(name, ".tf.json"))
			if !override {
				continue
			}
		case strings.HasSuffix(name, ".tf"):
			override = isOverride(strings.TrimSuffix(name, ".tf"))
		default:
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var blocks []fileBlock
		if strings.HasSuffix(name, ".json") {
			blocks, err = jsonModuleBlocks(parser, src, path)
		} else {
			blocks, err = nativeBlocks(parser, src, path)
		}
		if err != nil {
			return nil, err
		}
		for _, b := range blocks {
			switch {
			case b.kind == "variable" && !override:
				if _, seen := cfg.variables[b.label]; !seen {
					cfg.variables[b.label] = name
				}
			case b.kind == "module" && override:
				if overrides[b.label] == nil {
					overrides[b.label] = map[string]bool{}
				}
				for arg := range b.args {
					overrides[b.label][arg] = true
				}
			case b.kind == "module":
				// Terraform refuses two module blocks with one label; merging them keeps that error Terraform's to report.
				m, seen := cfg.modules[b.label]
				if !seen {
					m = module{file: name, args: map[string]bool{}}
				}
				for arg := range b.args {
					m.args[arg] = true
				}
				cfg.modules[b.label] = m
			}
		}
	}
	for label, args := range overrides {
		if m, ok := cfg.modules[label]; ok {
			for arg := range args {
				m.args[arg] = true
			}
		}
	}
	return cfg, nil
}

type fileBlock struct {
	kind, label string
	args        map[string]bool
}

func nativeBlocks(parser *hclparse.Parser, src []byte, path string) ([]fileBlock, error) {
	file, diags := parser.ParseHCL(src, path)
	if diags.HasErrors() {
		return nil, diags
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("%s: not native HCL syntax", path)
	}
	var blocks []fileBlock
	for _, block := range body.Blocks {
		if len(block.Labels) != 1 || (block.Type != "variable" && block.Type != "module") {
			continue
		}
		args := map[string]bool{}
		for name := range block.Body.Attributes {
			args[name] = true
		}
		blocks = append(blocks, fileBlock{kind: block.Type, label: block.Labels[0], args: args})
	}
	return blocks, nil
}

func jsonModuleBlocks(parser *hclparse.Parser, src []byte, path string) ([]fileBlock, error) {
	file, diags := parser.ParseJSON(src, path)
	if diags.HasErrors() {
		return nil, diags
	}
	content, _, diags := file.Body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "module", LabelNames: []string{"name"}}},
	})
	if diags.HasErrors() {
		return nil, diags
	}
	var blocks []fileBlock
	for _, block := range content.Blocks {
		attrs, diags := block.Body.JustAttributes()
		if diags.HasErrors() {
			return nil, diags
		}
		args := map[string]bool{}
		for name := range attrs {
			args[name] = true
		}
		blocks = append(blocks, fileBlock{kind: "module", label: block.Labels[0], args: args})
	}
	return blocks, nil
}

// isOverride is Terraform's rule for override files, given a file name without its .tf or .tf.json extension.
func isOverride(base string) bool {
	return base == "override" || strings.HasSuffix(base, "_override")
}

// ignoredFile is Terraform's rule for files it skips when loading a directory: hidden files (including the AppleDouble
// files macOS writes when a project is copied to another volume) and editor backup and lock files.
func ignoredFile(name string) bool {
	return strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, "~") ||
		(strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#"))
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
		if e.IsDir() || ignoredFile(name) {
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
		// Terraform reads a "//" key in a JSON variables file as a comment.
		if k == "//" {
			continue
		}
		keys = append(keys, k)
	}
	return keys, nil
}
