package controller

import (
	"context"
	"errors"
	"mygo-music/internal/audioengine/output"
	"testing"
	"time"
)

// Default monitoring also runs while paused, is rate limited, and a long
// observation interruption requires reopening rather than resuming old state.
func TestEndpointHealth(t *testing.T) {
	now := time.Now()
	calls := 0
	changed := false
	n := &nativeTransport{followDefault: true, device: &output.Stream{Info: output.Opened{EndpointID: "fixed"}}}
	n.probe = func(ctx context.Context, id string, follow bool) error {
		calls++
		if id != "fixed" || !follow {
			t.Fatal(id, follow)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded health probe")
		}
		if changed {
			return output.ErrDefaultChanged
		}
		return nil
	}
	if err := n.checkHealth(now); err != nil {
		t.Fatal(err)
	}
	if err := n.checkHealth(now.Add(500 * time.Millisecond)); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	changed = true
	if err := n.checkHealth(now.Add(2 * time.Second)); !errors.Is(err, output.ErrDefaultChanged) || calls != 2 {
		t.Fatal(err, calls)
	}
	if err := n.checkHealth(now.Add(8 * time.Second)); !errors.Is(err, output.ErrResumeRequired) || calls != 2 {
		t.Fatal(err, calls)
	}
}
