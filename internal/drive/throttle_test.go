package drive

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// TestThrottledReaderLimitsRate verifies that reading more than one burst of
// data through a limited reader is paced to roughly the configured rate.
func TestThrottledReaderLimitsRate(t *testing.T) {
	// 128 KB/s. The bucket starts with ~1s (128 KB) of burst, so to observe
	// throttling we must read beyond that. Read 192 KB: 128 KB is free, the
	// remaining 64 KB is paced at 128 KB/s ≈ 0.5s.
	SetUploadLimitKBps(128)
	defer SetUploadLimitKBps(0)

	data := bytes.Repeat([]byte("x"), 192*1024)
	r := limitUploadReader(bytes.NewReader(data))

	start := time.Now()
	n, err := io.Copy(io.Discard, r)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("copied %d bytes, want %d", n, len(data))
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("throttle too fast: %v (expected ~0.5s)", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("throttle too slow: %v", elapsed)
	}
}

// TestUnlimitedIsPassthrough verifies that when no limit is set the original
// reader is returned unwrapped (zero overhead).
func TestUnlimitedIsPassthrough(t *testing.T) {
	SetUploadLimitKBps(0)
	SetDownloadLimitKBps(0)

	orig := bytes.NewReader([]byte("hello"))
	if got := limitUploadReader(orig); got != orig {
		t.Fatalf("expected passthrough of original reader, got %T", got)
	}
}
