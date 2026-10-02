// Package clitest runs Rudolph's cobra commands in tests the way an operator runs them: with arguments, a typed
// confirmation on stdin, and the output captured.
//
// The CLI's commands read the DynamoDB table and region from persistent flags that the root command (internal/cli)
// fills from the environment's configuration. Run adds those two flags to the command tree it is given, so a test can
// run e.g. rule.RuleCmd or config.ConfigCmd against DynamoDB Local (see ddblocal) without a deployment configuration.
// Do not import internal/cli in such tests: once a command has a parent, cobra executes the parent's root.
package clitest

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Env is where the commands write.
type Env struct {
	Table  string
	Region string
}

var runMu sync.Mutex

// Run executes root with args after resetting every flag of the command tree to its default (cobra keeps flag values
// between executions), feeding stdin (e.g. "yes\n" to confirm) and capturing stdout. Output on stderr is kept in
// stdout too, so warnings can be asserted on.
func Run(t *testing.T, env Env, root *cobra.Command, stdin string, args ...string) (output string, err error) {
	t.Helper()
	runMu.Lock()
	defer runMu.Unlock()

	if root.PersistentFlags().Lookup("dynamodb_table") == nil {
		root.PersistentFlags().String("dynamodb_table", "", "DynamoDB table (added by clitest)")
	}
	if root.PersistentFlags().Lookup("region") == nil {
		root.PersistentFlags().String("region", "", "AWS region (added by clitest)")
	}
	resetFlags(root)
	if err := root.PersistentFlags().Set("dynamodb_table", env.Table); err != nil {
		t.Fatal(err)
	}
	if err := root.PersistentFlags().Set("region", env.Region); err != nil {
		t.Fatal(err)
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdinW, stdin); err != nil {
		t.Fatal(err)
	}
	stdinW.Close()

	origStdin, origStdout, origStderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdinR, outW, outW
	var captured bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&captured, outR)
		close(done)
	}()
	defer func() {
		os.Stdin, os.Stdout, os.Stderr = origStdin, origStdout, origStderr
		outW.Close()
		<-done
		stdinR.Close()
		outR.Close()
		output = captured.String()
	}()

	root.SetArgs(args)
	root.SetOut(outW)
	root.SetErr(outW)
	root.SilenceUsage = true
	root.SilenceErrors = true
	_, err = root.ExecuteC()
	return "", err
}

// resetFlags sets every flag of cmd and its subcommands back to its default and marks it as not given.
func resetFlags(cmd *cobra.Command) {
	reset := func(f *pflag.Flag) {
		if f.Name == "help" {
			f.Changed = false
			return
		}
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			var def []string
			if d := strings.Trim(f.DefValue, "[]"); d != "" {
				def = strings.Split(d, ",")
			}
			_ = slice.Replace(def)
		} else if err := f.Value.Set(f.DefValue); err != nil {
			// A custom flag type that cannot parse its own default (e.g. an enum whose default is ""): zero it.
			if v := reflect.ValueOf(f.Value); v.Kind() == reflect.Pointer && !v.IsNil() && v.Elem().CanSet() {
				v.Elem().Set(reflect.Zero(v.Elem().Type()))
			}
		}
		f.Changed = false
	}
	cmd.Flags().VisitAll(reset)
	cmd.PersistentFlags().VisitAll(reset)
	for _, sub := range cmd.Commands() {
		resetFlags(sub)
	}
}
