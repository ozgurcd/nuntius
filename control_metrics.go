package nuntius

import (
	"os"
	"sync/atomic"
	"time"
)

// Opt in before starting the process. Atomic, fixed storage adds no locks,
// goroutines, run identifiers, paths or private values to instrumentation.
var controlMetricsEnabled = os.Getenv("NUNTIUS_CONTROL_METRICS") == "1"
var controlHistograms [5][2][8]atomic.Uint64

func controlMetric(site, kind int, elapsed time.Duration) {
	if !controlMetricsEnabled {
		return
	}
	bucket := 0
	for _, bound := range []time.Duration{time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond, 250 * time.Millisecond, time.Second, 2 * time.Second} {
		if elapsed <= bound {
			break
		}
		bucket++
	}
	controlHistograms[site][kind][bucket].Add(1)
}

func controlMetricsSnapshot() any {
	if !controlMetricsEnabled {
		return map[string]any{"enabled": false}
	}
	rows := map[string]any{}
	for site, name := range []string{"writer", "shared_reader", "admission", "mutex", "metadata_mutex"} {
		var counts [2][8]uint64
		for kind := range counts {
			for bucket := range counts[kind] {
				counts[kind][bucket] = controlHistograms[site][kind][bucket].Load()
			}
		}
		rows[name] = map[string]any{"wait": counts[0], "hold": counts[1]}
	}
	return map[string]any{"enabled": true, "bucket_upper_ms": []int{1, 5, 10, 50, 250, 1000, 2000, -1}, "sites": rows}
}
