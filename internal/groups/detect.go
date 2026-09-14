package groups

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Detect reads the files a project uses to *declare* what it runs, and returns
// the services they state outright.
//
// It is deliberately small. A package manager's dev script and a compose
// file's services are declarations; a Makefile target, a Procfile line or an
// entry point buried in a framework is a guess about someone else's
// repository, and a table of those guesses is never finished. `sonar init
// --agent` is what reads the rest, with judgement sonar does not have.
func Detect(root string) []Service {
	var out []Service
	if svc, ok := detectNodeScript(root); ok {
		out = append(out, svc)
	}
	return append(out, detectCompose(root)...)
}

// nodeScripts is the order package.json scripts are preferred in: the one a
// developer runs to work on the project.
var nodeScripts = []string{"dev", "start"}

// detectNodeScript reads package.json's dev script, run by whichever package
// manager the lockfile commits the project to.
func detectNodeScript(root string) (Service, bool) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return Service{}, false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return Service{}, false
	}
	for _, name := range nodeScripts {
		if strings.TrimSpace(pkg.Scripts[name]) == "" {
			continue
		}
		return Service{Name: name, Cmd: packageManager(root) + " run " + name}, true
	}
	return Service{}, false
}

// packageManager is the one this project's lockfile commits it to.
func packageManager(root string) string {
	for _, lock := range []struct{ file, pm string }{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lockb", "bun"},
		{"bun.lock", "bun"},
	} {
		if _, err := os.Stat(filepath.Join(root, lock.file)); err == nil {
			return lock.pm
		}
	}
	return "npm"
}

// composeFiles are the names Docker Compose looks for, in its own order.
var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// detectCompose reads a compose file's services: one service each, started the
// way compose starts it, with the port it publishes and the order it declares.
// Services come back in name order — a YAML mapping has none of its own.
func detectCompose(root string) []Service {
	for _, name := range composeFiles {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		var file struct {
			Services map[string]struct {
				Ports     []yaml.Node `yaml:"ports"`
				DependsOn yaml.Node   `yaml:"depends_on"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &file); err != nil {
			return nil
		}
		names := make([]string, 0, len(file.Services))
		for n := range file.Services {
			names = append(names, n)
		}
		sort.Strings(names)

		out := make([]Service, 0, len(names))
		for _, n := range names {
			entry := file.Services[n]
			svc := Service{Name: n, Cmd: "docker compose up " + n}
			for _, p := range entry.Ports {
				if port := hostPort(p); port > 0 {
					svc.Port = port
					break
				}
			}
			svc.DependsOn = dependsOnNames(entry.DependsOn)
			out = append(out, svc)
		}
		return out
	}
	return nil
}

// hostPort reads the host side of one compose port entry, in either of the
// forms compose accepts: a string mapping, or the long syntax's `published`.
func hostPort(n yaml.Node) int {
	switch n.Kind {
	case yaml.ScalarNode:
		return hostPortOf(n.Value)
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "published" {
				return firstPort(n.Content[i+1].Value)
			}
		}
	}
	return 0
}

// hostPortOf reads "8080:80" and "127.0.0.1:8080:80" as 8080. A bare "80"
// publishes nothing fixed — compose picks the host port — so it is no port at
// all as far as a service declaration goes.
func hostPortOf(s string) int {
	parts := strings.Split(strings.Trim(s, `"'`), ":")
	if len(parts) < 2 {
		return 0
	}
	return firstPort(parts[len(parts)-2])
}

// firstPort reads a port, taking the first of a range and ignoring a protocol
// suffix: "3000-3005" and "3000/tcp" are both 3000.
func firstPort(s string) int {
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	if i := strings.IndexAny(s, "-/"); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

// dependsOnNames reads depends_on in either form: a list of names, or a
// mapping of names to conditions.
func dependsOnNames(n yaml.Node) []string {
	var out []string
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if item.Value != "" {
				out = append(out, item.Value)
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value != "" {
				out = append(out, n.Content[i].Value)
			}
		}
	}
	return out
}

// Merge adds the services a file declares to the ones sonar saw listening.
// The listening ones win: an open port is evidence, and the command behind it
// is the one that actually produced it. A declaration matching a live service
// by name or by port is folded into it rather than repeated, filling in only
// what the live one does not know.
func Merge(cfg *Config, detected []Service) *Config {
	out := *cfg
	out.Services = append([]Service{}, cfg.Services...)

	byName := map[string]int{}
	byPort := map[int]int{}
	for i, s := range out.Services {
		byName[s.Name] = i
		if s.Port != 0 {
			byPort[s.Port] = i
		}
	}

	for _, d := range detected {
		if i, ok := byName[d.Name]; ok {
			fill(&out.Services[i], d)
			continue
		}
		if d.Port != 0 {
			if i, ok := byPort[d.Port]; ok {
				fill(&out.Services[i], d)
				continue
			}
		}
		byName[d.Name] = len(out.Services)
		if d.Port != 0 {
			byPort[d.Port] = len(out.Services)
		}
		out.Services = append(out.Services, d)
	}

	pruneDependsOn(&out)
	return &out
}

// fill takes from a declaration only what the live service does not have.
func fill(live *Service, declared Service) {
	if live.Cmd == "" {
		live.Cmd = declared.Cmd
	}
	if live.Port == 0 {
		live.Port = declared.Port
	}
	if len(live.DependsOn) == 0 {
		live.DependsOn = declared.DependsOn
	}
}

// pruneDependsOn drops a depends_on naming a service the merge did not keep:
// the file has to load, and a dangling name is a validation error.
func pruneDependsOn(cfg *Config) {
	known := make(map[string]bool, len(cfg.Services))
	for _, s := range cfg.Services {
		known[s.Name] = true
	}
	for i := range cfg.Services {
		if len(cfg.Services[i].DependsOn) == 0 {
			continue
		}
		kept := make([]string, 0, len(cfg.Services[i].DependsOn))
		for _, dep := range cfg.Services[i].DependsOn {
			if known[dep] && dep != cfg.Services[i].Name {
				kept = append(kept, dep)
			}
		}
		if len(kept) == 0 {
			kept = nil
		}
		cfg.Services[i].DependsOn = kept
	}
}
