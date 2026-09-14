package cmd

import (
	"fmt"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
)

// requireConfigSupport refuses to hand a file to a daemon older than the
// format it uses.
//
// The daemon parses `sonar.yaml` itself, so one that predates `port: auto`
// answers with "cannot unmarshal !!str `auto` into int" — an error about the
// file, for a problem with the daemon. This says the true thing instead, and
// what to do about it. A daemon from before capabilities were announced at all
// is treated the same way, which is right: it is older still.
func requireConfigSupport(c *client.Client, cfg *groups.Config) error {
	if cfg == nil || !cfg.UsesAssignedPorts() {
		return nil
	}
	hello := c.Hello()
	for _, capability := range hello.Capabilities {
		if capability == rpc.CapabilityAutoPorts {
			return nil
		}
	}
	version := hello.DaemonVersion
	if version == "" {
		version = "an older version"
	}
	return fmt.Errorf("%s uses `port: auto`, which the running daemon (%s) does not know\nhint: restart it with `sonar daemon restart`",
		shortPath(cfg.Path), version)
}
