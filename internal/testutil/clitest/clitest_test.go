package clitest

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

type enumFlag struct {
	value string
	set   bool
}

func (e *enumFlag) Set(v string) error {
	if v != "a" && v != "b" {
		return errors.New("must be a or b")
	}
	e.value, e.set = v, true
	return nil
}
func (e *enumFlag) String() string { return e.value }
func (e *enumFlag) Type() string   { return "enum" }

func tree() (*cobra.Command, *bool, *[]string, *enumFlag) {
	root := &cobra.Command{Use: "root"}
	var global bool
	var list []string
	enum := &enumFlag{}
	sub := &cobra.Command{
		Use: "sub",
		RunE: func(cmd *cobra.Command, args []string) error {
			table, _ := cmd.Flags().GetString("dynamodb_table")
			region, _ := cmd.Flags().GetString("region")
			fmt.Printf("table=%s region=%s global=%t list=%v enum=%q set=%t\n", table, region, global, list, enum.value, enum.set)
			fmt.Fprintln(os.Stderr, "a warning")
			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(answer) != "yes" {
				return errors.New("not confirmed")
			}
			return nil
		},
	}
	sub.Flags().BoolVar(&global, "global", false, "")
	sub.Flags().StringSliceVar(&list, "list", nil, "")
	sub.Flags().Var(enum, "enum", "")
	root.AddCommand(sub)
	return root, &global, &list, enum
}

func TestRunPassesStdinCapturesOutputAndResetsFlags(t *testing.T) {
	root, _, _, _ := tree()
	env := Env{Table: "t1", Region: "us-east-1"}

	out, err := Run(t, env, root, "yes\n", "sub", "--global", "--list", "x,y", "--enum", "b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "table=t1 region=us-east-1 global=true list=[x y] enum=\"b\" set=true") || !strings.Contains(out, "a warning") {
		t.Fatalf("output = %q", out)
	}

	out, err = Run(t, env, root, "no\n", "sub")
	if err == nil || err.Error() != "not confirmed" {
		t.Fatalf("stdin not passed: %v", err)
	}
	if !strings.Contains(out, "global=false list=[] enum=\"\" set=false") {
		t.Fatalf("flags were not reset between runs: %q", out)
	}
}
