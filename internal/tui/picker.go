package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Device mirrors card.Device, and is copied rather than imported for the same
// reason Step is: tui draws what it is handed and knows nothing about where
// it came from.
type Device struct {
	Path      string
	Size      int64
	Name      string
	Removable bool
}

// ErrNoDevice is what PickDevice returns when the operator backed out. It is
// not a failure — nothing has been written — so the caller says so in its own
// words rather than reporting an error from a display.
var ErrNoDevice = errors.New("no disk chosen")

// PickDevice asks which disk to write, and returns it. The list is the
// caller's: this only draws it, moves the highlight and hands one back.
func PickDevice(ctx context.Context, devices []Device) (Device, error) {
	return pickWith(ctx, devices, tea.WithoutSignalHandler())
}

// pickWith is PickDevice with the program options spelled out, so tests can
// drive it through pipes instead of a terminal.
func pickWith(ctx context.Context, devices []Device, opts ...tea.ProgramOption) (Device, error) {
	if len(devices) == 0 {
		return Device{}, errors.New("no removable disk to choose from")
	}
	opts = append([]tea.ProgramOption{tea.WithContext(ctx)}, opts...)
	final, err := tea.NewProgram(&picker{devices: devices, chosen: -1}, opts...).Run()
	if err != nil {
		return Device{}, err
	}
	p, ok := final.(*picker)
	if !ok || p.chosen < 0 {
		return Device{}, ErrNoDevice
	}
	return p.devices[p.chosen], nil
}

// picker is the list. Like model, it owns no I/O: every field moves in
// Update, so the tests drive it with key messages.
type picker struct {
	devices []Device
	cursor  int
	chosen  int // -1 until enter
	width   int
}

func (p *picker) Init() tea.Cmd { return nil }

func (p *picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if p.cursor > 0 {
				p.cursor--
			}
		case "down", "j":
			if p.cursor < len(p.devices)-1 {
				p.cursor++
			}
		case "home", "g":
			p.cursor = 0
		case "end", "G":
			p.cursor = len(p.devices) - 1
		case "enter":
			p.chosen = p.cursor
			return p, tea.Quit
		case "q", "esc", "ctrl+c":
			// chosen stays -1: leaving the list is how the operator says no,
			// and it has to be as easy as picking.
			return p, tea.Quit
		}
	}
	return p, nil
}

func (p *picker) View() string {
	w := p.width
	if w < minWidth {
		w = minWidth
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(" Which disk should the image be written to?") + "\n")
	b.WriteString(failStyle.Render(" Everything on the disk you choose is erased.") + "\n\n")

	nameW := 4
	for _, d := range p.devices {
		if n := len(shortName(d.Path)); n > nameW {
			nameW = n
		}
	}
	for i, d := range p.devices {
		row := fmt.Sprintf("%-*s  %9s  %s", nameW, shortName(d.Path), diskSize(d.Size), join(d.Name, removableText(d)))
		if i == p.cursor {
			b.WriteString(runningStyle.Render(" > "+truncate(row, w-3)) + "\n")
			continue
		}
		b.WriteString("   " + truncate(row, w-3) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render(" ↑/↓ moves · enter writes to the highlighted disk · q cancels") + "\n")
	return b.String()
}

// shortName is how diskutil and the operator say it: disk4, not /dev/disk4.
func shortName(path string) string {
	if rest, ok := strings.CutPrefix(path, "/dev/"); ok {
		return rest
	}
	return path
}

// diskSize renders a disk in decimal GB. Cards are sold, labelled and
// reported by diskutil that way, so a row that said 29.7 GiB would not match
// anything else the operator can see.
func diskSize(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// removableText marks the disks that are what this command is for. A fixed
// external disk is still offered — it may be a reader that does not say so —
// but it does not get the word.
func removableText(d Device) string {
	if d.Removable {
		return "removable"
	}
	return ""
}
