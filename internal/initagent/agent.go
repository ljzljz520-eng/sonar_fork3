// Package initagent hands `sonar init` over to the coding agent the user
// already has.
//
// It opens that agent in this terminal with the prompt already sent, the way
// the user would have typed it: every file the agent reads and every edit it
// makes then goes through the agent's own permission prompts, which is exactly
// where that decision belongs. sonar contributes the prompt and, afterwards,
// the verdict on what was written.
package initagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Agent is a coding agent CLI and how an opening prompt reaches it.
type Agent struct {
	// Name is what `--agent` takes.
	Name string
	// Bin is the executable to look for on PATH.
	Bin string
	// Args come before the prompt, for an agent whose interactive mode is a
	// subcommand (`opencode run <prompt>`).
	Args []string
}

// Known are the agents sonar can start, in the order it prefers them.
var Known = []Agent{
	{Name: "claude", Bin: "claude"},
	{Name: "codex", Bin: "codex"},
	{Name: "cursor-agent", Bin: "cursor-agent"},
	{Name: "opencode", Bin: "opencode", Args: []string{"run"}},
}

// LookPath finds an executable. Tests replace it.
type LookPath func(string) (string, error)

// ErrNoAgent is returned when none of the known agents is installed.
var ErrNoAgent = errors.New("no coding agent found on PATH")

// UnknownAgentError names an agent sonar does not know how to start.
type UnknownAgentError struct{ Name string }

func (e *UnknownAgentError) Error() string {
	return fmt.Sprintf("unknown agent %q: sonar knows %s", e.Name, strings.Join(agentNames(Known), ", "))
}

// AmbiguousError is returned when several agents are installed and none was
// named: which one to hand the repository to is the user's call.
type AmbiguousError struct{ Installed []Agent }

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("several agents are installed (%s); name one with --agent <name>",
		strings.Join(agentNames(e.Installed), ", "))
}

// Installed lists the known agents present on PATH.
func Installed(look LookPath) []Agent {
	if look == nil {
		look = exec.LookPath
	}
	var out []Agent
	for _, a := range Known {
		if _, err := look(a.Bin); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// Resolve picks the agent to run: the one named, or the only one installed.
func Resolve(name string, look LookPath) (Agent, error) {
	if look == nil {
		look = exec.LookPath
	}
	installed := Installed(look)
	if name != "" && name != "auto" {
		for _, a := range Known {
			if a.Name != name {
				continue
			}
			if _, err := look(a.Bin); err != nil {
				return Agent{}, fmt.Errorf("%s is not on PATH", a.Bin)
			}
			return a, nil
		}
		return Agent{}, &UnknownAgentError{Name: name}
	}
	switch len(installed) {
	case 0:
		return Agent{}, ErrNoAgent
	case 1:
		return installed[0], nil
	default:
		return Agent{}, &AmbiguousError{Installed: installed}
	}
}

// Run starts the agent in dir with prompt as its first message, attached to
// this terminal so the user sees the session and answers its prompts. extra is
// passed through ahead of the prompt, which is how a caller reaches flags
// sonar knows nothing about.
func Run(ctx context.Context, a Agent, dir, prompt string, extra []string) error {
	args := make([]string, 0, len(a.Args)+len(extra)+1)
	args = append(args, a.Args...)
	args = append(args, extra...)
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, a.Bin, args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", a.Bin, err)
	}
	return nil
}

func agentNames(agents []Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Name)
	}
	sort.Strings(out)
	return out
}
