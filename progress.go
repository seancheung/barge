package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// layerState tracks one blob (config or layer) for display.
//
// `downloaded` is the absolute byte count currently on disk for this blob —
// cache hits and partial resumes are already reflected here at startup. It is
// what drives the progress bar / percentage.
//
// `transferred` counts bytes actually read from the network during this run.
// The rate calculation is based on `transferred`, not `downloaded`, so that
// recognizing a multi-GB cached blob does not produce a bogus speed spike.
type layerState struct {
	digest      string
	size        int64
	downloaded  atomic.Int64
	transferred atomic.Int64
	done        atomic.Bool
	errMsg      atomic.Pointer[string]
}

func newLayerState(digest string, size int64) *layerState {
	return &layerState{digest: digest, size: size}
}

// seedFromDisk primes a layerState's downloaded counter from the current
// on-disk state of dstPath (or its .part sibling). This runs before the
// tracker starts so the first rendered frame already reflects any cached or
// resumable bytes, avoiding a 0 → cache-size jump in the rate calculation.
func seedFromDisk(s *layerState, dstPath string) {
	if fi, err := os.Stat(dstPath); err == nil {
		s.downloaded.Store(fi.Size())
		return
	}
	if fi, err := os.Stat(dstPath + ".part"); err == nil {
		s.downloaded.Store(fi.Size())
	}
}

// progressCallback returns a progress callback suited for client.DownloadBlob.
// Each invocation receives the absolute bytes-on-disk count; the returned
// closure stores it on `downloaded` for display and adds only forward
// movement to `transferred` so the rate calculation excludes pre-existing
// cached bytes. prev is captured per-callback and is safe because
// DownloadBlob drives a single blob from a single goroutine.
func progressCallback(s *layerState) func(int64) {
	prev := s.downloaded.Load()
	return func(total int64) {
		if total > prev {
			s.transferred.Add(total - prev)
		}
		prev = total
		s.downloaded.Store(total)
	}
}

func (s *layerState) setDone() { s.done.Store(true) }

func (s *layerState) setError(err error) {
	msg := err.Error()
	s.errMsg.Store(&msg)
}

func (s *layerState) status() string {
	if s.done.Load() {
		return "done"
	}
	if s.errMsg.Load() != nil {
		return "err"
	}
	d := s.downloaded.Load()
	switch {
	case d == 0:
		return "wait"
	case d >= s.size && s.size > 0:
		return "verify"
	default:
		return "pull"
	}
}

// progressTracker owns stderr and redraws the progress frame on a timer.
// Retries are deliberately silent — if they keep succeeding, the only visible
// effect is continued progress; if the budget is exhausted, the returned error
// is surfaced by the caller.
type progressTracker struct {
	config  *layerState
	layers  []*layerState
	tty     bool
	done    chan struct{}
	stopped chan struct{}
}

func newProgressTracker(config *layerState, layers []*layerState) *progressTracker {
	return &progressTracker{
		config:  config,
		layers:  layers,
		tty:     isTerminal(os.Stderr),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Stop signals the printer loop and blocks until the final frame is drawn.
// Safe to call exactly once.
func (p *progressTracker) Stop() {
	close(p.done)
	<-p.stopped
}

// stats bundles the derived rate/elapsed/ETA values passed to renderers.
type stats struct {
	rate    float64       // bytes per second, smoothed
	elapsed time.Duration // since Run started
	eta     time.Duration // 0 when unknown
}

// Run drives the renderer until Stop is called or ctx is done.
func (p *progressTracker) Run(ctx context.Context) {
	defer close(p.stopped)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	start := time.Now()
	var (
		prevLines int
		lastTr    int64
		lastTick  = start
		rate      float64
	)

	computeStats := func() stats {
		now := time.Now()
		curDl, total := p.totals()
		curTr := p.totalTransferred()
		dt := now.Sub(lastTick).Seconds()
		if dt > 0 {
			delta := curTr - lastTr
			if delta < 0 {
				delta = 0
			}
			instant := float64(delta) / dt
			if rate == 0 {
				rate = instant
			} else {
				rate = 0.3*instant + 0.7*rate
			}
		}
		lastTr = curTr
		lastTick = now
		var eta time.Duration
		if rate > 0 && curDl < total {
			eta = time.Duration(float64(total-curDl) / rate * float64(time.Second))
		}
		return stats{rate: rate, elapsed: now.Sub(start), eta: eta}
	}

	draw := func() {
		st := computeStats()
		if p.tty {
			if prevLines > 0 {
				fmt.Fprintf(os.Stderr, "\033[%dA", prevLines)
			}
			prevLines = p.render(os.Stderr, st)
		} else {
			p.renderSummary(os.Stderr, st)
		}
	}

	for {
		select {
		case <-p.done:
			draw()
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			draw()
		}
	}
}

// render writes the multi-line frame (TTY). Returns number of lines written.
//
// Layout:
//   config     [....] bytes pct status
//   <digest>   [....] bytes pct status
//   ...
//   total      [....] bytes pct
//              <rate>/s  elapsed <t>  ETA <t>
func (p *progressTracker) render(w io.Writer, st stats) int {
	lines := 0
	if p.config != nil {
		fmt.Fprintf(w, "%s  %s\033[K\n", padRight("config", 8), lineFor(p.config))
		lines++
	}
	for _, l := range p.layers {
		fmt.Fprintf(w, "%s  %s\033[K\n", padRight(shortDigest(l.digest), 8), lineFor(l))
		lines++
	}
	sumDl, sumSize := p.totals()
	fmt.Fprintf(w, "%s  %s\033[K\n", padRight("total", 8), totalLine(sumDl, sumSize))
	lines++
	fmt.Fprintf(w, "%s  %s\033[K\n", padRight("", 8), statsLine(st))
	lines++
	return lines
}

// renderSummary writes one line with overall progress (non-TTY).
func (p *progressTracker) renderSummary(w io.Writer, st stats) {
	sumDl, sumSize := p.totals()
	pct := 0.0
	if sumSize > 0 {
		pct = float64(sumDl) / float64(sumSize) * 100
	}
	fmt.Fprintf(w, "downloading %s / %s (%.1f%%)  %s\n",
		humanBytes(sumDl), humanBytes(sumSize), pct, statsLine(st))
}

func (p *progressTracker) totals() (int64, int64) {
	var dl, total int64
	if p.config != nil {
		dl += p.config.downloaded.Load()
		total += p.config.size
	}
	for _, l := range p.layers {
		dl += l.downloaded.Load()
		total += l.size
	}
	return dl, total
}

// totalTransferred returns the sum of bytes actually read from the network so
// far across all tracked blobs. This excludes bytes that were already on disk
// from cache hits or prior partial downloads.
func (p *progressTracker) totalTransferred() int64 {
	var t int64
	if p.config != nil {
		t += p.config.transferred.Load()
	}
	for _, l := range p.layers {
		t += l.transferred.Load()
	}
	return t
}

func lineFor(s *layerState) string {
	d := s.downloaded.Load()
	pct := 0.0
	if s.size > 0 {
		pct = float64(d) / float64(s.size) * 100
	}
	return fmt.Sprintf("%s %9s / %-9s %5.1f%%  %s",
		bar(d, s.size, 24),
		humanBytes(d),
		humanBytes(s.size),
		pct,
		s.status())
}

func totalLine(dl, total int64) string {
	pct := 0.0
	if total > 0 {
		pct = float64(dl) / float64(total) * 100
	}
	return fmt.Sprintf("%s %9s / %-9s %5.1f%%",
		bar(dl, total, 24), humanBytes(dl), humanBytes(total), pct)
}

func statsLine(st stats) string {
	return fmt.Sprintf("%s/s  elapsed %s  ETA %s",
		humanBytes(int64(st.rate)),
		formatDuration(st.elapsed),
		formatETA(st.eta))
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	return d.Round(time.Second).String()
}

func formatETA(d time.Duration) string {
	if d <= 0 {
		return "--"
	}
	return formatDuration(d)
}

func bar(dl, total int64, width int) string {
	if width < 2 {
		width = 2
	}
	ratio := 0.0
	if total > 0 {
		ratio = float64(dl) / float64(total)
	}
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(width))
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 8 {
		return d[:8]
	}
	return d
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// isTerminal reports whether f is a TTY-like device.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
