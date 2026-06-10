package metrics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dolthub/eventkit"
)

const (
	AppName     = "beads"
	dataDirName = ".beads"

	EnvDisableMetrics    = "BD_DISABLE_METRICS"
	EnvDisableEventFlush = "BD_DISABLE_EVENT_FLUSH"

	DefaultPostHogAPIKey = "phc_BamBWZVk64mXQ5j2koom4tBRpKL8hz8XAc29EJ2WHmBU"
)

var (
	enabled bool
	apiKey  string
)

func Enabled() bool {
	return enabled
}

func APIKey() string {
	return apiKey
}

func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dataDirName, "eventsData"), nil
}

func Init(version string, enable bool, postHogAPIKey string) (func(context.Context), error) {
	enabled = enable
	apiKey = postHogAPIKey

	var emitter eventkit.Emitter = eventkit.NullEmitter{}
	if enabled {
		dir, err := DataDir()
		if err != nil {
			return func(context.Context) {}, fmt.Errorf("metrics: resolve data dir: %w", err)
		}
		fe, err := eventkit.NewFileEmitter(dir)
		if err != nil {
			return func(context.Context) {}, fmt.Errorf("metrics: file emitter: %w", err)
		}
		emitter = fe
	}

	c := eventkit.NewCollector(emitter,
		eventkit.WithDistinctID(eventkit.MachineID(AppName)),
		eventkit.WithAppName(AppName),
		eventkit.WithAppVersion(version),
		eventkit.WithDisabled(func() bool { return !enabled }),
	)
	eventkit.SetGlobal(c)

	return func(ctx context.Context) {
		_ = c.Close(ctx)
	}, nil
}

func Global() *eventkit.Collector {
	return eventkit.Global()
}

func NewCommandEvent(command string) *eventkit.Event {
	if command == "" {
		panic("metrics.NewCommandEvent: command is required")
	}
	evt := eventkit.NewEvent("cli_command")
	evt.SetAttribute("command", command)
	return evt
}
