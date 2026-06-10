package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/metrics"
)

func TestEnsureUserConfigExistsCreatesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := ensureUserConfigExists(); err != nil {
		t.Fatalf("ensureUserConfigExists: %v", err)
	}

	path := filepath.Join(home, ".config", "bd", "config.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, metrics.DefaultPostHogAPIKey) {
		t.Errorf("file missing api key: %q", got)
	}
	if !strings.Contains(got, "posthog:") {
		t.Errorf("file missing posthog namespace: %q", got)
	}
}

func TestEnsureUserConfigExistsLeavesExistingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".config", "bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	original := []byte("some: existing\nstuff: here\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := ensureUserConfigExists(); err != nil {
		t.Fatalf("ensureUserConfigExists: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("existing file was modified.\nwant: %q\ngot:  %q", original, got)
	}
}

func TestEnsureUserConfigExistsMkdirFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	blocker := filepath.Join(home, ".config")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	err := ensureUserConfigExists()
	if err == nil {
		t.Fatal("expected error when parent dir cannot be created, got nil")
	}
	if !strings.Contains(err.Error(), "user config") {
		t.Errorf("unexpected error: %v", err)
	}
}
