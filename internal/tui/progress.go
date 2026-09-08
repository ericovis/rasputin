package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// stepProgress is one step as the overall bar sees it: an estimate, a status,
// and — for the step that is running — how far in it is.
type stepProgress struct {
	Estimate time.Duration
	Status   stepStatus
	Elapsed  time.Duration // how long the running step has been running
	Fraction float64       // known byte fraction, < 0 when unknown
}

// stallCap is how far a running step may advance on the clock alone. Without
// bytes to go on, guessing past this reads as a finished step that is stuck.
const stallCap = 0.95

// overallFraction returns how much of the whole run is done and how much
// estimated time is left. Skipped steps count for nothing on either side of
// the ratio: a run that skips prepare and bake is not 0% done, it is short.
func overallFraction(steps []stepProgress) (float64, time.Duration) {
	var total, done time.Duration
	for _, s := range steps {
		if s.Status == statusSkipped || s.Estimate <= 0 {
			continue
		}
		total += s.Estimate
		switch s.Status {
		case statusDone, statusFailed:
			done += s.Estimate
		case statusRunning:
			f := s.Fraction
			if f < 0 || f > 1 {
				f = float64(s.Elapsed) / float64(s.Estimate)
			}
			// A step that is still running is never finished, however good
			// its bytes look: the last stretch is verification, not download.
			if f > stallCap {
				f = stallCap
			}
			if f > 0 {
				done += time.Duration(float64(s.Estimate) * f)
			}
		}
	}
	if total <= 0 {
		return 0, 0
	}
	if done > total {
		done = total
	}
	return float64(done) / float64(total), total - done
}

// humanBytes renders a byte count the way an operator reads a card: MiB up to
// a gigabyte, GiB above it.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// humanRate is MB/s, the unit the README measures the wire in.
func humanRate(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1f MB/s", bytesPerSec/1e6)
}

// clock is mm:ss, or h:mm:ss once a run passes an hour.
func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	mnt := int(d/time.Minute) % 60
	sec := int(d/time.Second) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, mnt, sec)
	}
	return fmt.Sprintf("%02d:%02d", mnt, sec)
}

// shortDur is the per-step duration column: seconds while it is seconds.
func shortDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	return clock(d)
}

// leftText is the "~19 min left" estimate. It rounds hard on purpose: these
// are measured averages, not predictions.
func leftText(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("~%ds left", int(d.Round(time.Second)/time.Second))
	default:
		return fmt.Sprintf("~%d min left", int((d+30*time.Second)/time.Minute))
	}
}

// pad right-pads to w columns, measuring what the terminal will show.
func pad(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// truncate cuts to w columns with an ellipsis, so a long node message never
// wraps the layout at 80 columns.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	if w == 1 {
		return "…"
	}
	return string(r[:min(len(r), w-1)]) + "…"
}

// join glues non-empty cells with two spaces, which is the whole layout rule.
func join(cells ...string) string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		if c != "" {
			out = append(out, c)
		}
	}
	return strings.Join(out, "  ")
}

// durationOf turns seconds into a Duration, guarding the divide-by-zero and
// overflow cases a stalled transfer produces.
func durationOf(seconds float64) time.Duration {
	if seconds <= 0 || seconds > 24*3600 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
