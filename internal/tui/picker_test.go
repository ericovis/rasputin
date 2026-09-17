package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

var readers = []Device{
	{Path: "/dev/disk4", Size: 31914983424, Name: "SD Card Reader", Removable: true},
	{Path: "/dev/disk5", Size: 128035676160, Name: "USB Flash Drive", Removable: true},
}

// pickInTest drives pickWith through pipes instead of a terminal, the way
// runInTest drives the sync display.
func pickInTest(t *testing.T, keys string) (Device, error) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	go pw.Write([]byte(keys))
	return pickWith(context.Background(), readers,
		tea.WithInput(pr), tea.WithOutput(&syncWriter{}), tea.WithoutSignalHandler())
}

// press applies key messages the way the bubbletea loop would. Movement is
// driven through the model rather than through the pipe because a terminal
// delivers one keystroke per message while a pipe delivers a whole burst as
// one, which is not what a hand on a keyboard does.
func press(p *picker, keys ...tea.KeyMsg) *picker {
	for _, k := range keys {
		next, _ := p.Update(k)
		p = next.(*picker)
	}
	return p
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestPickDeviceReturnsTheHighlightedDisk(t *testing.T) {
	got, err := pickInTest(t, "\r")
	if err != nil {
		t.Fatalf("PickDevice: %v", err)
	}
	if got.Path != "/dev/disk4" {
		t.Errorf("chose %q, want the first disk", got.Path)
	}
}

func TestPickDeviceMovesTheHighlight(t *testing.T) {
	p := press(&picker{devices: readers, chosen: -1},
		tea.KeyMsg{Type: tea.KeyDown}, key("k"), key("j"))
	if p.cursor != 1 {
		t.Errorf("cursor = %d after down, up, down, want the second disk", p.cursor)
	}
	if p = press(p, tea.KeyMsg{Type: tea.KeyEnter}); p.chosen != 1 {
		t.Errorf("chosen = %d after enter, want the highlighted disk", p.chosen)
	}
}

// TestPickDeviceStopsAtTheEnds: the highlight is what enter erases, so it
// must never wrap from the last row onto the first.
func TestPickDeviceStopsAtTheEnds(t *testing.T) {
	p := press(&picker{devices: readers, chosen: -1}, key("j"), key("j"), key("j"))
	if p.cursor != len(readers)-1 {
		t.Errorf("cursor = %d after running off the bottom, want the last disk", p.cursor)
	}
	if p = press(p, key("k"), key("k"), key("k")); p.cursor != 0 {
		t.Errorf("cursor = %d after running off the top, want the first disk", p.cursor)
	}
}

// TestPickDeviceCancels: leaving the list is how the operator says no, and it
// has to reach the caller as something it can report as "nothing was written".
func TestPickDeviceCancels(t *testing.T) {
	for _, keys := range []string{"q", "\x1b"} {
		if _, err := pickInTest(t, keys); !errors.Is(err, ErrNoDevice) {
			t.Errorf("cancelling with %q = %v, want ErrNoDevice", keys, err)
		}
	}
}

func TestPickDeviceWithNothingToPick(t *testing.T) {
	if _, err := pickWith(context.Background(), nil); err == nil {
		t.Fatal("PickDevice on an empty list returned no error")
	}
}

// TestPickerViewShowsWhatIsAtStake: the row has to be recognisable as the
// disk in `diskutil list`, and the warning has to be on screen before the
// first keystroke.
func TestPickerViewShowsWhatIsAtStake(t *testing.T) {
	view := (&picker{devices: readers, chosen: -1, width: 80}).View()
	for _, want := range []string{"erased", "disk4", "31.9 GB", "SD Card Reader", "removable", "disk5", "q cancels"} {
		if !strings.Contains(view, want) {
			t.Errorf("the picker does not show %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "/dev/disk4") {
		t.Errorf("the rows spell out /dev/, which diskutil does not:\n%s", view)
	}
}

func TestDiskSizeIsWhatTheCardSays(t *testing.T) {
	for n, want := range map[int64]string{
		31914983424:  "31.9 GB",
		128035676160: "128.0 GB",
		536870912:    "537 MB",
		512:          "512 B",
	} {
		if got := diskSize(n); got != want {
			t.Errorf("diskSize(%d) = %q, want %q", n, got, want)
		}
	}
}
