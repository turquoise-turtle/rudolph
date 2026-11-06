package flags

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const (
	yesFlag    = "yes"
	dryRunFlag = "dry-run"
)

// ErrNotConfirmed is returned by Approval.Confirm when the answer is not "yes" or "ok", including when stdin is empty,
// so that a script can tell a declined change from an applied one by the exit code
var ErrNotConfirmed = errors.New("not confirmed; nothing was changed (pass --yes to apply without asking)")

// AddYesFlag adds the persistent --yes (-y) flag to a command whose subcommands ask for confirmation before writing
func AddYesFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().BoolP(yesFlag, "y", false, `Apply the changes without asking for confirmation (for scripts)`)
}

// AddDryRunFlag adds the persistent --dry-run flag to a command whose subcommands write. Every write is decided by the
// Approval of ApprovalOf (Confirm, or Write for one applied without asking), which honours it.
func AddDryRunFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().Bool(dryRunFlag, false, `Read and check everything as usual, then print what would be written instead of writing it; commands that only read ignore it`)
}

// Approval is how a command that writes was told to go ahead: with --yes, with --dry-run (not at all), or by asking
type Approval struct {
	Yes    bool
	DryRun bool
}

// ApprovalOf returns the --yes and --dry-run flags given to cmd or one of its parents
func ApprovalOf(cmd *cobra.Command) Approval {
	return Approval{Yes: flagSet(cmd, yesFlag), DryRun: flagSet(cmd, dryRunFlag)}
}

func flagSet(cmd *cobra.Command, name string) bool {
	set, err := cmd.Flags().GetBool(name)
	return err == nil && set
}

// Confirm decides whether to apply the changes just printed. In a dry run it says nothing was written and returns
// false without asking; with --yes it returns true; otherwise it asks, and the answer "yes" or "ok" (in any case)
// returns true, any other answer, or none, ErrNotConfirmed.
func (a Approval) Confirm() (apply bool, err error) {
	if a.DryRun {
		printDryRun()
		return false, nil
	}
	if a.Yes {
		return true, nil
	}
	fmt.Println(`Apply changes? (Enter: "yes" or "ok")`)
	fmt.Print("> ")
	text, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Println()
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "yes", "ok":
		return true, nil
	}
	return false, ErrNotConfirmed
}

// Write decides whether to apply a change just printed that is applied without asking: false in a dry run, after
// saying nothing was written
func (a Approval) Write() bool {
	if a.DryRun {
		printDryRun()
		return false
	}
	return true
}

func printDryRun() {
	fmt.Println("Dry run: the changes shown are not written.")
}
