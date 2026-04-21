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
type layerState struct {
	digest     string
	size       int64
	downloaded atomic.Int64
	done       atomic.Bool
	errMsg     atomic.Pointer[string]
}

func newLayerState(digest string, size int64) *layerState {
	return &layerState{digest: digest, size: size}
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

// progressTracker coordinates the progress printer with async events (retries,
// log messages). The printer goroutine owns all stderr writes.
type progressTracker struct {
	config  *layerState
	layers  []*layerState
	msgs    chan string // out-of-band messages (retry notices, warnings)
	done    chan struct{}
	stopped chan struct{}
}

func newProgressTracker(config *layerState, layers []*layerState) *progressTracker {
	return &progressTracker{
		config:  config,
		layers:  layers,
		msgs:    make(chan string, 16),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Message queues a line to be printed above the progress bars on next tick.
func (p *progressTracker) Message(format string, args ...any) {
	select {
	case p.msgs <- fmt.Sprintf(format, args...):
	default:
		// channel full — drop silently rather than block downloaders
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
	tty := isTerminal(os.Stderr)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	start := time.Now()
	var (
		prevLines int
		lastDl    int64
		lastTick  = start
		rate      float64
	)

	computeStats := func() stats {
		now := time.Now()
		curDl, total := p.totals()
		dt := now.Sub(lastTick).Seconds()
		if dt > 0 {
			instant := float64(curDl-lastDl) / dt
			if rate == 0 {
				rate = instant
			} else {
				rate = 0.3*instant + 0.7*rate
			}
		}
		lastDl = curDl
		lastTick = now
		var eta time.Duration
		if rate > 0 && curDl < total {
			eta = time.Duration(float64(total-curDl) / rate * float64(time.Second))
		}
		return stats{rate: rate, elapsed: now.Sub(start), eta: eta}
	}
	clearFrame := func() {
		if tty && prevLines > 0 {
			fmt.Fprintf(os.Stderr, "\033[%dA\033[J", prevLines)
		}
		prevLines = 0
	}
	draw := func() {
		st := computeStats()
		if tty {
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
			clearFrame()
			st := computeStats()
			if tty {
				prevLines = p.render(os.Stderr, st)
			} else {
				p.renderSummary(os.Stderr, st)
			}
			return
		case <-ctx.Done():
			return
		case msg := <-p.msgs:
			clearFrame()
			fmt.Fprintln(os.Stderr, msg)
			draw()
		case <-ticker.C:
			draw()
		}
	}
}

// render writes the multi-line bar chart (TTY). Returns number of lines written.
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
	fmt.Fprintf(w, "%s  %s\033[K\n", padRight("total", 8), totalLine(sumDl, sumSize, st))
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
	fmt.Fprintf(w, "downloading %s / %s (%.1f%%)  %s/s  elapsed %s  ETA %s\n",
		humanBytes(sumDl), humanBytes(sumSize), pct,
		humanBytes(int64(st.rate)), formatDuration(st.elapsed), formatETA(st.eta))
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

func totalLine(dl, total int64, st stats) string {
	pct := 0.0
	if total > 0 {
		pct = float64(dl) / float64(total) * 100
	}
	return fmt.Sprintf("%s %9s / %-9s %5.1f%%  %8s/s  elapsed %-6s  ETA %s",
		bar(dl, total, 24),
		humanBytes(dl),
		humanBytes(total),
		pct,
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
