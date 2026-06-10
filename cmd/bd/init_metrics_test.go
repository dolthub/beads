package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/metrics"
)

type metricsEvent struct {
	AppName string `json:"app_name"`
	Events  []struct {
		Name       string `json:"name"`
		Attributes []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"attributes"`
	} `json:"events"`
}

func readSingleInitEvent(t *testing.T, home string) metricsEvent {
	t.Helper()
	dir := filepath.Join(home, ".beads", "eventsData")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read eventsData dir: %v", err)
	}
	var evtqs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".evtq") {
			evtqs = append(evtqs, e.Name())
		}
	}
	if len(evtqs) != 1 {
		t.Fatalf("expected 1 .evtq file, got %d: %v", len(evtqs), evtqs)
	}
	body, err := os.ReadFile(filepath.Join(dir, evtqs[0]))
	if err != nil {
		t.Fatalf("read evtq: %v", err)
	}
	var got metricsEvent
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal evtq: %v\n%s", err, body)
	}
	return got
}

func (e metricsEvent) attr(key string) (string, bool) {
	if len(e.Events) == 0 {
		return "", false
	}
	for _, a := range e.Events[0].Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return "", false
}

func runBdInitForMetrics(t *testing.T, home string, args ...string) {
	t.Helper()
	bd := buildEmbeddedBD(t)
	repo, err := testTempDir("bd-metrics-repo-*")
	if err != nil {
		t.Fatalf("temp repo: %v", err)
	}
	initGitRepoAt(t, repo)

	full := append([]string{"init", "--non-interactive", "--quiet"}, args...)
	cmd := exec.Command(bd, full...)
	cmd.Dir = repo
	env := append([]string{}, bdEnv(home)...)
	env = append(env, "BD_DISABLE_EVENT_FLUSH=1")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
}

func TestInitMetricsEmittedPerDoltMode(t *testing.T) {
	cases := []struct {
		name             string
		extraArgs        []string
		extraEnv         []string
		expectedDoltMode string
	}{
		{
			name:             "embedded_default",
			expectedDoltMode: "embedded",
		},
		{
			name:             "server_via_flag",
			extraArgs:        []string{"--server"},
			expectedDoltMode: "server",
		},
		{
			name:             "shared_server_via_flag",
			extraArgs:        []string{"--shared-server"},
			expectedDoltMode: "shared-server",
		},
		{
			name:             "proxied_server_via_flag",
			extraArgs:        []string{"--proxied-server"},
			expectedDoltMode: "proxied-server",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home, err := testTempDir("bd-metrics-home-*")
			if err != nil {
				t.Fatalf("temp home: %v", err)
			}
			runBdInitForMetrics(t, home, tc.extraArgs...)
			evt := readSingleInitEvent(t, home)

			if evt.AppName != "beads" {
				t.Errorf("app_name = %q, want %q", evt.AppName, "beads")
			}
			if len(evt.Events) != 1 {
				t.Fatalf("events len = %d, want 1", len(evt.Events))
			}
			if evt.Events[0].Name != "cli_command" {
				t.Errorf("event name = %q, want %q", evt.Events[0].Name, "cli_command")
			}
			if got, _ := evt.attr("command"); got != "init" {
				t.Errorf("command attr = %q, want %q", got, "init")
			}
			if got, _ := evt.attr("dolt_mode"); got != tc.expectedDoltMode {
				t.Errorf("dolt_mode attr = %q, want %q", got, tc.expectedDoltMode)
			}
		})
	}
}

func writeUserMetricsDisabled(t *testing.T, home string, disabled bool) {
	t.Helper()
	dir := filepath.Join(home, ".config", "bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir user config: %v", err)
	}
	body := []byte("metrics.disabled: false\n")
	if disabled {
		body = []byte("metrics.disabled: true\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), body, 0o644); err != nil {
		t.Fatalf("write user config: %v", err)
	}
}

func evtqFilesIn(t *testing.T, home string) []string {
	t.Helper()
	dir := filepath.Join(home, ".beads", "eventsData")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".evtq") {
			out = append(out, e.Name())
		}
	}
	return out
}

func runBdInitWithEnv(t *testing.T, home string, extraEnv []string) {
	t.Helper()
	bd := buildEmbeddedBD(t)
	repo, err := testTempDir("bd-metrics-repo-*")
	if err != nil {
		t.Fatalf("temp repo: %v", err)
	}
	initGitRepoAt(t, repo)

	cmd := exec.Command(bd, "init", "--non-interactive", "--quiet")
	cmd.Dir = repo
	env := append([]string{}, bdEnv(home)...)
	env = append(env, "BD_DISABLE_EVENT_FLUSH=1")
	env = append(env, extraEnv...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
}

func TestInitMetricsEnvConfigPrecedence(t *testing.T) {
	cases := []struct {
		name           string
		setUserConfig  bool
		configDisabled bool
		envVar         []string
		wantEvtq       bool
	}{
		{
			name:     "default_no_config_no_env",
			wantEvtq: true,
		},
		{
			name:           "config_disabled_env_unset",
			setUserConfig:  true,
			configDisabled: true,
			wantEvtq:       false,
		},
		{
			name:           "config_enabled_env_disables",
			setUserConfig:  true,
			configDisabled: false,
			envVar:         []string{"BD_DISABLE_METRICS=1"},
			wantEvtq:       false,
		},
		{
			name:           "config_disabled_env_overrides_to_enabled",
			setUserConfig:  true,
			configDisabled: true,
			envVar:         []string{"BD_DISABLE_METRICS=0"},
			wantEvtq:       true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home, err := testTempDir("bd-metrics-prec-*")
			if err != nil {
				t.Fatalf("temp home: %v", err)
			}
			if tc.setUserConfig {
				writeUserMetricsDisabled(t, home, tc.configDisabled)
			}

			runBdInitWithEnv(t, home, tc.envVar)

			files := evtqFilesIn(t, home)
			if tc.wantEvtq && len(files) == 0 {
				t.Errorf("expected an .evtq file, got none")
			}
			if !tc.wantEvtq && len(files) > 0 {
				t.Errorf("expected no .evtq files, got %v", files)
			}
		})
	}
}

func TestInitBootstrapsUserConfigWhenMissing(t *testing.T) {
	home, err := testTempDir("bd-bootstrap-fresh-*")
	if err != nil {
		t.Fatalf("temp home: %v", err)
	}

	runBdInitWithEnv(t, home, nil)

	path := filepath.Join(home, ".config", "bd", "config.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := string(body)
	if !strings.Contains(got, metrics.DefaultPostHogAPIKey) {
		t.Errorf("user config missing api key.\nfile: %q", got)
	}
	if !strings.Contains(got, "posthog:") {
		t.Errorf("user config missing posthog namespace.\nfile: %q", got)
	}
}

func TestInitLeavesExistingUserConfigUntouched(t *testing.T) {
	home, err := testTempDir("bd-bootstrap-existing-*")
	if err != nil {
		t.Fatalf("temp home: %v", err)
	}

	dir := filepath.Join(home, ".config", "bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	original := []byte("# user customizations\nmetrics:\n  posthog:\n    api_key: phc_custom_user_key\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runBdInitWithEnv(t, home, nil)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("existing user config was modified.\nwant: %q\ngot:  %q", original, got)
	}
}

func TestInitMetricsDisabledSuppresses(t *testing.T) {
	bd := buildEmbeddedBD(t)
	home, err := testTempDir("bd-metrics-disabled-home-*")
	if err != nil {
		t.Fatalf("temp home: %v", err)
	}
	repo, err := testTempDir("bd-metrics-disabled-repo-*")
	if err != nil {
		t.Fatalf("temp repo: %v", err)
	}
	initGitRepoAt(t, repo)

	cmd := exec.Command(bd, "init", "--non-interactive", "--quiet")
	cmd.Dir = repo
	env := append([]string{}, bdEnv(home)...)
	env = append(env, "BD_DISABLE_METRICS=1")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bd init failed: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}

	dir := filepath.Join(home, ".beads", "eventsData")
	if _, err := os.Stat(dir); err == nil {
		entries, _ := os.ReadDir(dir)
		var evtqs []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".evtq") {
				evtqs = append(evtqs, e.Name())
			}
		}
		if len(evtqs) > 0 {
			t.Errorf("BD_DISABLE_METRICS=1 still produced .evtq files: %v", evtqs)
		}
	}
}
