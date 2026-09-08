package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// lipglossWidth is the width of a rendered line, ANSI excluded.
func lipglossWidth(s string) int { return lipgloss.Width(s) }

func TestOverallFraction(t *testing.T) {
	min := time.Minute
	tests := []struct {
		name  string
		steps []stepProgress
		frac  float64
		left  time.Duration
	}{
		{
			name:  "nothing started",
			steps: []stepProgress{{Estimate: min}, {Estimate: 3 * min}},
			frac:  0,
			left:  4 * min,
		},
		{
			name:  "skipped steps shorten the run instead of stalling it",
			steps: []stepProgress{{Estimate: min, Status: statusSkipped}, {Estimate: 3 * min, Status: statusDone}},
			frac:  1,
			left:  0,
		},
		{
			name: "bytes beat the clock for a running step",
			steps: []stepProgress{
				{Estimate: min, Status: statusDone},
				{Estimate: min, Status: statusRunning, Elapsed: 59 * time.Second, Fraction: 0.5},
			},
			frac: 0.75,
			left: 30 * time.Second,
		},
		{
			name: "without bytes a running step creeps on the clock",
			steps: []stepProgress{
				{Estimate: 2 * min, Status: statusRunning, Elapsed: min, Fraction: -1},
			},
			frac: 0.5,
			left: min,
		},
		{
			name: "a slow step stops short of full",
			steps: []stepProgress{
				{Estimate: min, Status: statusRunning, Elapsed: 10 * min, Fraction: -1},
			},
			frac: stallCap,
			left: 3 * time.Second,
		},
		{
			name:  "a failed step still counts as spent time",
			steps: []stepProgress{{Estimate: min, Status: statusFailed}, {Estimate: min}},
			frac:  0.5,
			left:  min,
		},
		{
			name:  "no estimates at all",
			steps: []stepProgress{{Status: statusRunning}},
			frac:  0,
			left:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frac, left := overallFraction(tt.steps)
			if diff := frac - tt.frac; diff > 0.01 || diff < -0.01 {
				t.Errorf("fraction = %.3f, want %.3f", frac, tt.frac)
			}
			if got := left.Round(time.Second); got != tt.left {
				t.Errorf("time left = %s, want %s", got, tt.left)
			}
		})
	}
}

func TestHumanFormats(t *testing.T) {
	tests := []struct {
		got, want string
	}{
		{humanBytes(4 << 30), "4.0 GiB"},
		{humanBytes(1536 << 20), "1.5 GiB"},
		{humanBytes(700 << 20), "700 MiB"},
		{humanBytes(512), "512 B"},
		{humanRate(9.8e6), "9.8 MB/s"},
		{humanRate(0), ""},
		{clock(391 * time.Second), "06:31"},
		{clock(2*time.Hour + 5*time.Minute), "2:05:00"},
		{shortDur(2 * time.Second), "2s"},
		{shortDur(64 * time.Second), "01:04"},
		{leftText(19 * time.Minute), "~19 min left"},
		{leftText(45 * time.Second), "~45s left"},
		{leftText(0), ""},
		{truncate("rasputin001 is capturing", 10), "rasputin0…"},
		{truncate("short", 10), "short"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}
