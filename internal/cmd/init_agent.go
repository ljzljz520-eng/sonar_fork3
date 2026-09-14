package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/initagent"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

// runInitAgent hands the file to the user's own coding agent: it opens the
// agent here, with the prompt already sent, and checks what came back.
//
// sonar deliberately stops at the prompt. Reading an unfamiliar repository and
// deciding what a developer runs is judgement, and a table of per-stack
// guesses in this repository would never be finished; the agent has the
// judgement, and the user watching it has the say over every edit.
func runInitAgent(cmd *cobra.Command, root string, cfg *groups.Config, live []ports.ListeningPort, name string, extra []string) error {
	draft, err := groups.Marshal(cfg)
	if err != nil {
		return err
	}
	prompt, err := initagent.Prompt(initagent.Context{
		Root:       root,
		ConfigPath: groups.TargetIn(root),
		Legacy:     legacyConfig(root),
		Listening:  listeningLines(root, live),
		Draft:      withoutHeader(string(draft)),
		Sonar:      sonarBinary(),
		HasSkill:   skillInstalled(root),
	})
	if err != nil {
		return err
	}

	agent, resolveErr := initagent.Resolve(name, nil)
	// A choice the user has to make — which of several agents, or a name that
	// is not one sonar knows — is an error, not a wall of prompt.
	if resolveErr != nil && !errors.Is(resolveErr, initagent.ErrNoAgent) {
		return resolveErr
	}
	if resolveErr != nil || !interactiveTerminal() {
		// Nothing to hand it to, or nobody watching: the prompt is still the
		// useful thing, so it goes to stdout to be piped or pasted.
		if resolveErr != nil {
			fmt.Fprintf(os.Stderr, "%s; printing the prompt instead\n\n", resolveErr)
		} else {
			fmt.Fprintf(os.Stderr, "not a terminal; printing the prompt instead of starting %s\n\n", agent.Name)
		}
		fmt.Println(prompt)
		return nil
	}

	fmt.Fprintf(os.Stderr, "%s %s in %s\n\n",
		display.Dim("starting"), agent.Name, shortPath(root))
	if err := initagent.Run(cmd.Context(), agent, root, prompt, extra); err != nil {
		return err
	}
	return reportInitResult(root)
}

// reportInitResult says what the agent left behind: the services the file
// declares, or why it does not load. A cancelled agent must not read as a
// finished migration, so both of those are failures.
func reportInitResult(root string) error {
	present := groups.FilesIn(root)
	if len(present) == 0 {
		return fmt.Errorf("no %s was written in %s", groups.ConfigName, shortPath(root))
	}
	cfg, err := groups.Load(present[0])
	if err != nil {
		return err
	}
	fmt.Printf("\n%s: group %s with %d %s\n",
		shortPath(cfg.Path), cfg.Name, len(cfg.Services), pluralWord(len(cfg.Services), "service"))
	for _, s := range cfg.Services {
		where := ""
		switch {
		case s.PortAuto:
			where = "auto"
		case s.Port != 0:
			where = strconv.Itoa(s.Port)
		}
		fmt.Printf("  %-16s %-6s %s\n", s.Name, where, s.Cmd)
	}
	return nil
}

// splitAgentArgs reads `sonar init --agent [name] [-- args…]`.
//
// The name comes before `--` rather than after the flag because a flag that is
// valid on its own cannot also swallow the next word: `--agent claude` leaves
// pflag with `--agent` set to its default and `claude` as an argument. So a
// single argument before `--` is the agent, and everything after `--` is the
// agent's own.
func splitAgentArgs(cmd *cobra.Command, args []string) (string, []string, error) {
	before, extra := args, []string(nil)
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		before, extra = args[:dash], args[dash:]
	}
	name := initAgentFlag
	switch {
	case len(before) == 0:
	case len(before) > 1:
		return "", nil, fmt.Errorf("name one agent: `sonar init --agent %s`", before[0])
	case name != "auto":
		return "", nil, fmt.Errorf("the agent is named twice: --agent %s and %s", name, before[0])
	default:
		name = before[0]
	}
	return name, extra, nil
}

// sonarBinary is this executable, for the checks the prompt asks the agent to
// run. A binary that cannot name itself falls back to whatever `sonar` means
// on PATH, which is better than nothing.
func sonarBinary() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return resolved
		}
		return exe
	}
	return "sonar"
}

// legacyConfig is an older `.sonar.yaml` in this project, so the prompt can
// say to rename it rather than leave two files behind.
func legacyConfig(root string) string {
	for _, path := range groups.FilesIn(root) {
		if groups.IsLegacyName(filepath.Base(path)) {
			return path
		}
	}
	return ""
}

// listeningLines renders what is open inside the project right now: the
// evidence the agent should trust over anything a file implies.
func listeningLines(root string, live []ports.ListeningPort) []string {
	var out []string
	for _, p := range live {
		dir := p.Cwd
		if dir == "" {
			dir = p.ProjectRoot
		}
		if dir == "" || !insideDir(dir, root) {
			continue
		}
		line := fmt.Sprintf("%d  %s", p.Port, p.DisplayName())
		if cmd := strings.TrimSpace(p.Command); cmd != "" {
			line += "  (" + cmd + ")"
		}
		out = append(out, line)
	}
	return out
}

// insideDir reports whether dir is inside root.
func insideDir(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// skillInstalled reports whether sonar's agent skill is where an agent would
// find it, so the prompt can point at it instead of repeating it.
func skillInstalled(root string) bool {
	candidates := []string{filepath.Join(root, ".claude", "skills", "sonar", "SKILL.md")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".claude", "skills", "sonar", "SKILL.md"))
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// withoutHeader drops the generated comment block from a rendered config, so
// the draft inside the prompt is the services and nothing else.
func withoutHeader(yaml string) string {
	lines := strings.Split(yaml, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "#") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return yaml
}

// interactiveTerminal reports whether there is a user watching: an agent is
// started for someone to answer its prompts.
func interactiveTerminal() bool {
	in, err := os.Stdin.Stat()
	if err != nil || in.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	out, err := os.Stdout.Stat()
	return err == nil && out.Mode()&os.ModeCharDevice != 0
}
