package ui

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Progress tracks and displays progress for a multi-step operation.
type Progress struct {
	w       io.Writer
	mu      sync.Mutex // guards writes to w in non-verbose mode
	total   int
	done    atomic.Int64
	label   string
	verbose bool
	start   time.Time
	dirty   bool // true if a \r line is currently displayed
}

// NewProgress creates a progress tracker writing to w.
func NewProgress(w io.Writer, label string, total int, verbose bool) *Progress {
	return &Progress{
		w:       w,
		total:   total,
		label:   label,
		verbose: verbose,
		start:   time.Now(),
	}
}

// Increment records one completed item. If verbose, prints the item name.
func (p *Progress) Increment(name string) {
	n := p.done.Add(1)
	if p.verbose {
		fmt.Fprintf(p.w, "  %s %s\n", p.label, name)
		return
	}
	p.mu.Lock()
	fmt.Fprintf(p.w, "\r\033[K%s [%d/%d]", p.label, n, p.total)
	p.dirty = true
	p.mu.Unlock()
}

// Error prints an error, clearing the progress line first if needed.
func (p *Progress) Error(msg string) {
	p.mu.Lock()
	if p.dirty {
		fmt.Fprint(p.w, "\r\033[K") // clear progress line
	}
	fmt.Fprintln(p.w, msg)
	p.dirty = false
	p.mu.Unlock()
}

// Finish prints the final summary line.
func (p *Progress) Finish() {
	elapsed := time.Since(p.start)
	p.mu.Lock()
	if p.dirty {
		fmt.Fprintln(p.w) // newline after \r progress
		p.dirty = false
	}
	p.mu.Unlock()
	fmt.Fprintf(p.w, "%s done (%d packages, %s)\n", p.label, p.total, formatDuration(elapsed))
}

// PrintSummary prints a final timing summary.
func PrintSummary(w io.Writer, totalPackages int, start time.Time) {
	elapsed := time.Since(start)
	fmt.Fprintf(w, "\nvif install completed: %d packages in %s\n", totalPackages, formatDuration(elapsed))
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
