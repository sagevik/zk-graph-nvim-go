// Package zk runs the zk CLI.
package zk

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Graph runs `zk graph --format=json` in root. zk re-indexes changed notes first, so the
// output is always current. The zk LSP indexes on save too; a concurrent run can hit
// SQLite's "database is locked", which is retried with a short backoff.
func Graph(ctx context.Context, bin, root string, extra []string) ([]byte, error) {
	args := append([]string{"graph", "--format=json", "--no-input"}, extra...)
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 150 * time.Millisecond):
			}
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil {
			return out, nil
		}
		msg := strings.TrimSpace(stderr.String())
		lastErr = fmt.Errorf("zk graph: %v: %s", err, msg)
		if !strings.Contains(strings.ToLower(msg), "locked") {
			break
		}
	}
	return nil, lastErr
}
