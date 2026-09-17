package config

import "testing"

// Shortcoming L12: the CLI exposes the confirmation needed for fleet-wide LOCKDOWN.
func TestConfigCommands_HaveGlobalLockdownConfirmation_L12(t *testing.T) {
	for _, name := range []string{"set", "update"} {
		cmd, _, err := ConfigCmd.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("config %s command not found: %v", name, err)
		}
		if cmd.Flags().Lookup("confirm-global-lockdown") == nil {
			t.Errorf("config %s has no --confirm-global-lockdown flag", name)
		}
	}
}
