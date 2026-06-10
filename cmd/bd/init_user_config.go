package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/metrics"
)

func ensureUserConfigExists() error {
	path := config.UserConfigYamlPath()
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat user config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir user config: %w", err)
	}
	body := []byte("metrics:\n  posthog:\n    api_key: " + metrics.DefaultPostHogAPIKey + "\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write user config: %w", err)
	}
	return nil
}
