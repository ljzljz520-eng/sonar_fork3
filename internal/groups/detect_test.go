package groups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAt(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func serviceNames(services []Service) string {
	out := make([]string, 0, len(services))
	for _, s := range services {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

func TestDetectNodeScript(t *testing.T) {
	for _, tt := range []struct {
		name, scripts, lock, wantCmd string
	}{
		{"dev with npm", `{"dev":"vite","build":"vite build"}`, "", "npm run dev"},
		{"start when there is no dev", `{"start":"node server.js"}`, "", "npm run start"},
		{"dev wins over start", `{"start":"node server.js","dev":"vite"}`, "", "npm run dev"},
		{"pnpm from the lockfile", `{"dev":"vite"}`, "pnpm-lock.yaml", "pnpm run dev"},
		{"yarn from the lockfile", `{"dev":"vite"}`, "yarn.lock", "yarn run dev"},
		{"bun from the lockfile", `{"dev":"vite"}`, "bun.lock", "bun run dev"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAt(t, dir, "package.json", `{"scripts":`+tt.scripts+`}`)
			if tt.lock != "" {
				writeAt(t, dir, tt.lock, "")
			}
			got := Detect(dir)
			if len(got) != 1 || got[0].Cmd != tt.wantCmd {
				t.Fatalf("Detect = %+v, want one service running %q", got, tt.wantCmd)
			}
		})
	}
}

func TestDetectIgnoresAPackageWithNothingToRun(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "package.json", `{"scripts":{"build":"tsc","test":"vitest"}}`)
	if got := Detect(dir); len(got) != 0 {
		t.Errorf("Detect = %+v, want nothing: neither script runs the project", got)
	}
	writeAt(t, dir, "package.json", "not json at all")
	if got := Detect(dir); len(got) != 0 {
		t.Errorf("Detect = %+v, want nothing from a broken package.json", got)
	}
}

func TestDetectCompose(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "compose.yaml", `
services:
  web:
    image: nginx
    ports: ["8080:80"]
    depends_on: [api]
  api:
    image: api
    ports:
      - "127.0.0.1:8000:8000"
    depends_on:
      db:
        condition: service_healthy
  db:
    image: postgres
    ports:
      - target: 5432
        published: 5433
  worker:
    image: worker
  probe:
    image: probe
    ports: ["9000"]
`)
	got := Detect(dir)
	if serviceNames(got) != "api,db,probe,web,worker" {
		t.Fatalf("Detect = %s, want every compose service in name order", serviceNames(got))
	}
	byName := map[string]Service{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if s := byName["web"]; s.Port != 8080 || s.Cmd != "docker compose up web" {
		t.Errorf("web = %+v", s)
	}
	if s := byName["api"]; s.Port != 8000 {
		t.Errorf("api port = %d, want the host side of a three-part mapping", s.Port)
	}
	if s := byName["db"]; s.Port != 5433 {
		t.Errorf("db port = %d, want the long syntax's published port", s.Port)
	}
	if s := byName["probe"]; s.Port != 0 {
		t.Errorf("probe port = %d: a bare container port publishes nothing fixed", s.Port)
	}
	if s := byName["worker"]; s.Port != 0 || s.Cmd != "docker compose up worker" {
		t.Errorf("worker = %+v, want a service with no port", s)
	}
	if s := byName["web"]; strings.Join(s.DependsOn, ",") != "api" {
		t.Errorf("web depends_on = %v", s.DependsOn)
	}
	if s := byName["api"]; strings.Join(s.DependsOn, ",") != "db" {
		t.Errorf("api depends_on = %v, want the mapping form read", s.DependsOn)
	}
}

func TestDetectPrefersTheComposeFileComposeWouldUse(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "compose.yaml", "services:\n  first:\n    image: a\n")
	writeAt(t, dir, "docker-compose.yml", "services:\n  second:\n    image: b\n")
	if got := Detect(dir); serviceNames(got) != "first" {
		t.Errorf("Detect = %s, want compose.yaml to win", serviceNames(got))
	}
}

func TestMergeKeepsWhatIsListening(t *testing.T) {
	live := &Config{Name: "shop", Services: []Service{
		{Name: "web", Cmd: "vite", Port: 5173},
		{Name: "api", Port: 8000},
	}}
	detected := []Service{
		{Name: "web", Cmd: "npm run dev"},                               // same name
		{Name: "gateway", Cmd: "docker compose up gateway", Port: 8000}, // same port
		{Name: "db", Cmd: "docker compose up db", Port: 5432, DependsOn: []string{"gone"}},
	}
	got := Merge(live, detected)

	if serviceNames(got.Services) != "web,api,db" {
		t.Fatalf("services = %s, want the live two plus what is new", serviceNames(got.Services))
	}
	if got.Services[0].Cmd != "vite" {
		t.Errorf("web cmd = %q, want the command that is actually running", got.Services[0].Cmd)
	}
	if got.Services[1].Cmd != "docker compose up gateway" {
		t.Errorf("api cmd = %q, want the declaration to fill in what the live service lacks", got.Services[1].Cmd)
	}
	if len(got.Services[2].DependsOn) != 0 {
		t.Errorf("db depends_on = %v, want a dangling name dropped", got.Services[2].DependsOn)
	}
	if len(live.Services) != 2 || live.Services[0].Cmd != "vite" {
		t.Error("Merge changed the config it was given")
	}
}

func TestMergedConfigLoads(t *testing.T) {
	dir := mkdir(t, tempTree(t), "shop")
	writeAt(t, dir, "compose.yaml", "services:\n  db:\n    image: postgres\n    ports: [\"5432:5432\"]\n  api:\n    image: api\n    depends_on: [db]\n")
	writeAt(t, dir, "package.json", `{"scripts":{"dev":"vite"}}`)

	cfg := Merge(&Config{Name: "shop", Dir: dir, Path: filepath.Join(dir, ConfigName)}, Detect(dir))
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(filepath.Join(dir, ConfigName), data); err != nil {
		t.Fatalf("a detected config does not load: %v\n%s", err, data)
	}
}
