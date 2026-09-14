package initagent

import (
	_ "embed"
	"strings"
	"text/template"
)

// promptTemplate is the migration prompt, embedded so it is versioned with the
// format it teaches rather than fetched or written by hand at each site.
//
//go:embed PROMPT.md
var promptTemplate string

// Context is what the prompt is filled in with.
type Context struct {
	// Root is the project the agent is pointed at.
	Root string
	// ConfigPath is the file to write.
	ConfigPath string
	// Legacy is an older `.sonar.yaml` in the same project, or "".
	Legacy string
	// Listening is one line per port open inside the project right now.
	Listening []string
	// Draft is the YAML sonar would have written on its own.
	Draft string
	// HasSkill says the sonar agent skill is installed, so the prompt can
	// point at it instead of repeating it.
	HasSkill bool
}

// Prompt renders the migration prompt for a project.
func Prompt(c Context) (string, error) {
	t, err := template.New("prompt").Parse(promptTemplate)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, c); err != nil {
		return "", err
	}
	return b.String(), nil
}
