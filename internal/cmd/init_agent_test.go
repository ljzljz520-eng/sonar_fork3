package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// splitFor runs splitAgentArgs the way cobra would: ArgsLenAtDash is only set
// while a command is executing, so the parse has to go through one.
func splitFor(t *testing.T, flag string, argv ...string) (string, []string, error) {
	t.Helper()
	initAgentFlag = flag
	t.Cleanup(func() { initAgentFlag = "" })

	var (
		name  string
		extra []string
		err   error
	)
	c := &cobra.Command{
		Use:  "init",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, extra, err = splitAgentArgs(cmd, args)
			return nil
		},
	}
	c.SetArgs(argv)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if execErr := c.Execute(); execErr != nil {
		t.Fatalf("executing the test command: %v", execErr)
	}
	return name, extra, err
}

func TestSplitAgentArgs(t *testing.T) {
	t.Run("a bare name picks the agent", func(t *testing.T) {
		name, extra, err := splitFor(t, "auto", "claude")
		if err != nil || name != "claude" || len(extra) != 0 {
			t.Fatalf("= %q, %v, %v", name, extra, err)
		}
	})

	t.Run("the flag's own value is kept", func(t *testing.T) {
		name, _, err := splitFor(t, "codex")
		if err != nil || name != "codex" {
			t.Fatalf("= %q, %v", name, err)
		}
	})

	t.Run("everything after -- is the agent's", func(t *testing.T) {
		name, extra, err := splitFor(t, "auto", "claude", "--", "--allow-dangerously-skip-permissions")
		if err != nil || name != "claude" || strings.Join(extra, " ") != "--allow-dangerously-skip-permissions" {
			t.Fatalf("= %q, %v, %v", name, extra, err)
		}
	})

	t.Run("pass-through without a name", func(t *testing.T) {
		name, extra, err := splitFor(t, "auto", "--", "--model", "opus")
		if err != nil || name != "auto" || strings.Join(extra, " ") != "--model opus" {
			t.Fatalf("= %q, %v, %v", name, extra, err)
		}
	})

	t.Run("two names is a question, not a guess", func(t *testing.T) {
		if _, _, err := splitFor(t, "auto", "claude", "codex"); err == nil ||
			!strings.Contains(err.Error(), "name one agent") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("named twice", func(t *testing.T) {
		if _, _, err := splitFor(t, "codex", "claude"); err == nil ||
			!strings.Contains(err.Error(), "named twice") {
			t.Fatalf("err = %v", err)
		}
	})
}
