package cmd

import (
	"fmt"
	"strings"

	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/profile"
	"github.com/spf13/cobra"
)

// profileCmd is what is left of profiles: the one command that gets a project
// off them. A profile was a per-machine snapshot of ports; `sonar.yaml` is
// committed with the project, and everything else profiles could do is now
// done by groups.
var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Convert an old port profile into a " + groups.ConfigName,
}

// profileExportCmd is the migration path off profiles: it prints the
// `sonar.yaml` a profile would become, and never writes it. Which repository
// the services belong in is the user's call, not ours.
var profileExportCmd = &cobra.Command{
	Use:   "export <name>",
	Short: "Print the " + groups.ConfigName + " a profile would become",
	Long: "Convert a saved profile into a " + groups.ConfigName + " proposal on stdout.\n\n" +
		"Nothing is written: redirect it to the file yourself once you have\n" +
		"reviewed it, at the root of the repository the services belong to.\n\n" +
		"  sonar profile export my-app > " + groups.ConfigName,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		prof, err := profile.Load(args[0])
		if err != nil {
			return err
		}
		out, err := groups.Marshal(profileConfig(prof))
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "# Converted from the sonar profile %q by `sonar profile export`.\n", prof.Name)
		fmt.Fprint(cmd.OutOrStdout(), string(out))
		return nil
	},
}

// profileConfig converts a profile into a group config. A profile only knew
// ports, names and health paths — it never recorded how a service is started —
// so the proposal carries no `cmd:` and the user fills those in. Health comes
// across because that is the one field the two formats share.
func profileConfig(p *profile.Profile) *groups.Config {
	cfg := &groups.Config{Name: configName(p.Name)}
	used := map[string]bool{}
	for _, entry := range p.Ports {
		svc := groups.Service{
			Name: uniqueServiceName(entry.Name, entry.Port, used),
			Port: entry.Port,
		}
		switch {
		case entry.HealthPath != "":
			svc.Health = entry.HealthPath
		case entry.Health:
			svc.Health = "/"
		}
		cfg.Services = append(cfg.Services, svc)
	}
	return cfg
}

// configName makes a profile name usable as a group name: the validator
// rejects slashes and whitespace.
func configName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ' ' || r == '\t' || r == '\n' {
			return '-'
		}
		return r
	}, name)
	if name == "" {
		return "project"
	}
	return name
}

// uniqueServiceName sanitises a profile entry's display name into something the
// config validator accepts, and keeps it unique within the file.
func uniqueServiceName(candidate string, port int, used map[string]bool) string {
	var b strings.Builder
	for _, r := range strings.ToLower(candidate) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-._")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	if name == "" {
		name = fmt.Sprintf("service-%d", port)
	}
	base := name
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[name] = true
	return name
}

func init() {
	profileCmd.AddCommand(profileExportCmd)
	rootCmd.AddCommand(profileCmd)
}
