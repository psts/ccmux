package childproc

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestCount_SeesAnUnreapedChild is the point of the package: a process that was
// started and never waited on must show up as defunct. Uses count directly, not
// Count, to sidestep the cache.
func TestCount_SeesAnUnreapedChild(t *testing.T) {
	self := os.Getpid()
	before := count(self)
	if !before.Known {
		t.Skip("child census not supported on this platform")
	}

	// Exits immediately, and we deliberately do not Wait for it.
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() }) // reap it however the test ends

	var got Counts
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got = count(self); got.Defunct > before.Defunct {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Defunct <= before.Defunct {
		t.Fatalf("defunct count did not rise for an unreaped child: before=%d after=%d",
			before.Defunct, got.Defunct)
	}

	// And reaping it must bring the count back down — otherwise the number
	// only ever grows and says nothing.
	_ = cmd.Wait()
	for time.Now().Before(deadline) {
		if count(self).Defunct <= before.Defunct {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("defunct count stayed up after the child was reaped: %d", count(self).Defunct)
}

// TestCount_Caches keeps a hub that polls /v1/health from walking /proc on
// every request.
func TestCount_Caches(t *testing.T) {
	first := Count()
	mu.Lock()
	cached = Counts{Live: 4242, Defunct: 4242, Known: true} // poison the cache
	mu.Unlock()
	if again := Count(); again.Live != 4242 {
		t.Errorf("Count recomputed instead of using the cache: %+v (first %+v)", again, first)
	}
}
