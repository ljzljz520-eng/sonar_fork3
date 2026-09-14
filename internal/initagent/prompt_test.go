package initagent

import (
	"strings"
	"testing"
)

func TestPromptCarriesWhatSonarKnows(t *testing.T) {
	got, err := Prompt(Context{
		Root:       "/code/shop",
		ConfigPath: "/code/shop/sonar.yaml",
		Legacy:     "/code/shop/.sonar.yaml",
		Listening:  []string{"5173  vite (v5.4)", "8000  uvicorn app:app"},
		Draft:      "name: shop\nservices: []\n",
		HasSkill:   true,
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	for _, want := range []string{
		"/code/shop/sonar.yaml",
		".sonar.yaml",
		"5173  vite (v5.4)",
		"name: shop",
		"${api.url}",
		"port: auto",
		"`sonar` skill is installed",
		"Do not start, stop or kill anything",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt does not carry %q", want)
		}
	}
}

// TestPromptLeavesOutWhatThereIsNone: a project with nothing listening, no
// older file and no skill must not grow empty sections.
func TestPromptLeavesOutWhatThereIsNone(t *testing.T) {
	got, err := Prompt(Context{Root: "/code/shop", ConfigPath: "/code/shop/sonar.yaml", Draft: "name: shop\n"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	for _, absent := range []string{"Listening right now", ".sonar.yaml", "skill is installed"} {
		if strings.Contains(got, absent) {
			t.Errorf("prompt should not mention %q when there is none", absent)
		}
	}
}
