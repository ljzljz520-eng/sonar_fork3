package initagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installed builds a LookPath where exactly these binaries exist.
func installed(bins ...string) LookPath {
	have := map[string]bool{}
	for _, b := range bins {
		have[b] = true
	}
	return func(bin string) (string, error) {
		if have[bin] {
			return "/usr/local/bin/" + bin, nil
		}
		return "", errors.New("not found")
	}
}

func TestResolve(t *testing.T) {
	if _, err := Resolve("", installed()); !errors.Is(err, ErrNoAgent) {
		t.Errorf("no agent installed: err = %v, want ErrNoAgent", err)
	}

	got, err := Resolve("", installed("codex"))
	if err != nil || got.Name != "codex" {
		t.Errorf("one agent installed = %+v, %v; want codex", got, err)
	}

	var ambiguous *AmbiguousError
	_, err = Resolve("", installed("claude", "opencode"))
	if !errors.As(err, &ambiguous) {
		t.Fatalf("two agents installed: err = %v, want AmbiguousError", err)
	}
	if !strings.Contains(ambiguous.Error(), "--agent") {
		t.Errorf("error %q should say how to choose", ambiguous)
	}

	got, err = Resolve("opencode", installed("claude", "opencode"))
	if err != nil || got.Name != "opencode" {
		t.Errorf("named agent = %+v, %v", got, err)
	}

	if _, err := Resolve("claude", installed("codex")); err == nil ||
		!strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("naming an agent that is not installed: err = %v", err)
	}

	var unknown *UnknownAgentError
	if _, err := Resolve("emacs", installed("claude")); !errors.As(err, &unknown) {
		t.Errorf("unknown agent: err = %v, want UnknownAgentError", err)
	}
}

// TestRunSendsThePromptLast: the prompt is the agent's first message, and
// anything the caller passed through comes before it, where a flag belongs.
func TestRunSendsThePromptLast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub agent is a shell script")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "stub-agent")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\npwd >> %s\n", record, record)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()

	agent := Agent{Name: "stub", Bin: stub, Args: []string{"run"}}
	if err := Run(context.Background(), agent, project, "write the file", []string{"--yolo"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 4 {
		t.Fatalf("stub recorded %q", lines)
	}
	if lines[0] != "run" || lines[1] != "--yolo" || lines[2] != "write the file" {
		t.Errorf("argv = %q, want the subcommand, then the caller's flags, then the prompt", lines[:3])
	}
	if resolved, _ := filepath.EvalSymlinks(project); !strings.HasSuffix(lines[3], filepath.Base(resolved)) {
		t.Errorf("the agent ran in %q, want the project directory %q", lines[3], project)
	}
}

func TestRunReportsAnAgentThatFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub agent is a shell script")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub-agent")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Agent{Name: "stub", Bin: stub}, dir, "prompt", nil)
	if err == nil || !strings.Contains(err.Error(), "stub-agent") {
		t.Errorf("err = %v, want the agent named", err)
	}
}
