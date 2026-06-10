package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInitDisabledKeepsEnabledFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	closeFn, err := Init("0.0.0-test", false, "")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer closeFn(context.Background())

	if Enabled() {
		t.Fatalf("Enabled() = true, want false")
	}

	evt := NewCommandEvent("init")
	Global().CloseEventAndAdd(evt)
	closeFn(context.Background())

	dir := filepath.Join(home, ".beads", "eventsData")
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".evtq" {
				t.Errorf("disabled Init produced .evtq file: %s", e.Name())
			}
		}
	}
}

func TestInitEnabledFlipsEnabledTrue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	closeFn, err := Init("0.0.0-test", true, "")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer closeFn(context.Background())

	if !Enabled() {
		t.Fatalf("Enabled() = false, want true")
	}

	evt := NewCommandEvent("init")
	evt.SetAttribute("dolt_mode", "embedded")
	Global().CloseEventAndAdd(evt)
	closeFn(context.Background())

	dir := filepath.Join(home, ".beads", "eventsData")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read eventsData: %v", err)
	}
	var found bool
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".evtq" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("enabled Init did not produce any .evtq file in %s", dir)
	}
}

func TestRunSendMetricsNoOpWhenDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := Init("0.0.0-test", false, "")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if code := RunSendMetrics(); code != 0 {
		t.Errorf("RunSendMetrics() = %d, want 0", code)
	}
}

func TestMaybeSpawnFlusherNoOpWhenDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := Init("0.0.0-test", false, "")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	MaybeSpawnFlusher()
}
