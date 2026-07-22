package worker

import (
	"context"
	"testing"
	"time"
)

func TestRunnerSurvivesPanic(t *testing.T) {
	calls := 0
	r := NewRunner("test", 10*time.Millisecond, func(ctx context.Context) error {
		calls++
		if calls == 1 {
			panic("boom")
		}
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r.Start(ctx) // must return via ctx cancel, not die on the first-cycle panic

	if calls < 2 {
		t.Fatalf("calls = %d, want >= 2 (worker should continue after panic)", calls)
	}
}
