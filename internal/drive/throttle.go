package drive

import (
	"io"
	"sync"
	"time"
)

// throttle.go implements global, process-wide bandwidth limiting for Drive
// transfers. Limits are expressed in KB/s and apply across all sync pairs
// (total upload and total download are limited separately). A zero or negative
// limit means unlimited. Limits can be changed at runtime; changes take effect
// on subsequent reads.

// throttleChunk caps how many bytes a throttled reader consumes per Read, so
// throttling stays smooth instead of bursting a whole buffer then sleeping.
const throttleChunk = 32 * 1024

// uploadLimiter and downloadLimiter are the shared limiters consulted by every
// Drive client. They start unlimited.
var (
	uploadLimiter   = &bandwidthLimiter{}
	downloadLimiter = &bandwidthLimiter{}
)

// SetUploadLimitKBps sets the global upload bandwidth limit in KB/s (<=0 means
// unlimited). Safe to call at runtime.
func SetUploadLimitKBps(kbps int) { uploadLimiter.setKBps(kbps) }

// SetDownloadLimitKBps sets the global download bandwidth limit in KB/s (<=0
// means unlimited). Safe to call at runtime.
func SetDownloadLimitKBps(kbps int) { downloadLimiter.setKBps(kbps) }

// bandwidthLimiter is a simple token-bucket byte-rate limiter.
type bandwidthLimiter struct {
	mu          sync.Mutex
	bytesPerSec float64 // 0 = unlimited
	allowance   float64 // available tokens, in bytes
	last        time.Time
}

func (b *bandwidthLimiter) setKBps(kbps int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if kbps <= 0 {
		b.bytesPerSec = 0
	} else {
		b.bytesPerSec = float64(kbps) * 1024
	}
	b.allowance = b.bytesPerSec
	b.last = time.Now()
}

func (b *bandwidthLimiter) unlimited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytesPerSec <= 0
}

// wait blocks until n bytes may be consumed under the current rate, then
// records the consumption. It is a no-op when unlimited.
func (b *bandwidthLimiter) wait(n int) {
	b.mu.Lock()
	rate := b.bytesPerSec
	if rate <= 0 {
		b.mu.Unlock()
		return
	}
	now := time.Now()
	if b.last.IsZero() {
		b.last = now
	}
	b.allowance += now.Sub(b.last).Seconds() * rate
	b.last = now
	if b.allowance > rate { // cap burst to ~1 second of data
		b.allowance = rate
	}
	b.allowance -= float64(n)

	var sleep time.Duration
	if b.allowance < 0 {
		sleep = time.Duration(-b.allowance / rate * float64(time.Second))
	}
	b.mu.Unlock()

	if sleep > 0 {
		time.Sleep(sleep)
	}
}

// throttledReader wraps an io.Reader, pacing it to a bandwidthLimiter.
type throttledReader struct {
	r io.Reader
	l *bandwidthLimiter
}

func (t *throttledReader) Read(p []byte) (int, error) {
	if t.l.unlimited() {
		return t.r.Read(p)
	}
	if len(p) > throttleChunk {
		p = p[:throttleChunk]
	}
	n, err := t.r.Read(p)
	if n > 0 {
		t.l.wait(n)
	}
	return n, err
}

// throttledReadCloser adds Close passthrough for response bodies.
type throttledReadCloser struct {
	throttledReader
	c io.Closer
}

func (t *throttledReadCloser) Close() error { return t.c.Close() }

// limitUploadReader wraps an upload body with the global upload limiter.
func limitUploadReader(r io.Reader) io.Reader {
	if uploadLimiter.unlimited() {
		return r
	}
	return &throttledReader{r: r, l: uploadLimiter}
}

// limitDownloadReadCloser wraps a download body with the global download limiter.
func limitDownloadReadCloser(rc io.ReadCloser) io.ReadCloser {
	if downloadLimiter.unlimited() {
		return rc
	}
	return &throttledReadCloser{throttledReader: throttledReader{r: rc, l: downloadLimiter}, c: rc}
}
