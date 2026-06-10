package metrics

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dolthub/eventkit"
	posthogtx "github.com/dolthub/eventkit/transport/posthog"
)

const (
	EnvPostHogAPIKey = "BEADS_POSTHOG_API_KEY"

	flushTimeout = 30 * time.Second
)

func RunSendMetrics() int {
	if !Enabled() {
		return 0
	}

	key := APIKey()
	if key == "" {
		return 0
	}

	dir, err := DataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "send-metrics: %v\n", err)
		return 1
	}

	ph, err := posthogtx.New(posthogtx.Config{APIKey: key})
	if err != nil {
		fmt.Fprintf(os.Stderr, "send-metrics: posthog: %v\n", err)
		return 1
	}

	flusher := eventkit.NewFileFlusher(dir, ph)
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := flusher.Flush(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "send-metrics: flush: %v\n", err)
		return 1
	}
	return 0
}
