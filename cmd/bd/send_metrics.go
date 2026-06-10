package main

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/metrics"
)

var sendMetricsCmd = &cobra.Command{
	Use:    metrics.SendMetricsSubcommand,
	Short:  "Internal: flush queued telemetry events (spawned by bd)",
	Hidden: true,
	Args:   cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(metrics.RunSendMetrics())
	},
}

func init() {
	rootCmd.AddCommand(sendMetricsCmd)
}
