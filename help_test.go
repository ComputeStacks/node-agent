package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// TestHelpCoversCLI keeps the help text in step with the CLI: every
// subcommand and every flag must be documented. Add new commands to
// agentCommands / maintenanceCommands and new flags to registerAgentFlags /
// newMaintenanceFlagSet so this test sees them.
func TestHelpCoversCLI(t *testing.T) {
	for name := range agentCommands {
		if !strings.Contains(agentUsage, "cs-agent "+name) {
			t.Errorf("agentUsage does not document command %q", name)
		}
	}
	fs := flag.NewFlagSet("cs-agent", flag.ContinueOnError)
	registerAgentFlags(fs)
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(agentUsage, "-"+f.Name) {
			t.Errorf("agentUsage does not document flag -%s", f.Name)
		}
	})

	for _, cmd := range maintenanceCommands {
		if !strings.Contains(maintenanceUsage, "\n  "+cmd+" ") && !strings.Contains(maintenanceUsage, "\n  "+cmd+"\n") {
			t.Errorf("maintenanceUsage does not document command %q", cmd)
		}
		fs, _, _ := newMaintenanceFlagSet(cmd, io.Discard)
		fs.VisitAll(func(f *flag.Flag) {
			if !strings.Contains(maintenanceUsage, "--"+f.Name) {
				t.Errorf("maintenanceUsage does not document flag --%s (maintenance %s)", f.Name, cmd)
			}
		})
	}
}
