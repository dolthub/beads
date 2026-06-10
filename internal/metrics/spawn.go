package metrics

import (
	"os"
	"os/exec"
	"strings"
)

const SendMetricsSubcommand = "send-metrics"

func MaybeSpawnFlusher() {
	if !Enabled() || flushDisabledByEnv() {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, SendMetricsSubcommand)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return
	}
	_ = cmd.Process.Release()
}

func flushDisabledByEnv() bool {
	v := os.Getenv(EnvDisableEventFlush)
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "0", "false":
		return false
	}
	return true
}
