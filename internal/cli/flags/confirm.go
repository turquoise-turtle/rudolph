package flags

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const yesFlag = "yes"

// ErrNotConfirmed is returned by Confirm when the answer is not "yes" or "ok", including when stdin is empty, so that
// a script can tell a declined change from an applied one by the exit code
var ErrNotConfirmed = errors.New("not confirmed; nothing was changed (pass --yes to apply without asking)")

// AddYesFlag adds the persistent --yes (-y) flag to a command whose subcommands ask for confirmation before writing
func AddYesFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().BoolP(yesFlag, "y", false, `Apply the changes without asking for confirmation (for scripts)`)
}

// AssumeYes reports whether --yes was given to cmd or one of its parents
func AssumeYes(cmd *cobra.Command) bool {
	yes, err := cmd.Flags().GetBool(yesFlag)
	return err == nil && yes
}

// Confirm asks whether to apply the changes just printed, unless yes is true. The answer "yes" or "ok" is accepted in
// any case; any other answer, or none, returns ErrNotConfirmed.
func Confirm(yes bool) error {
	if yes {
		return nil
	}
	fmt.Println(`Apply changes? (Enter: "yes" or "ok")`)
	fmt.Print("> ")
	text, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Println()
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "yes", "ok":
		return nil
	}
	return ErrNotConfirmed
}
