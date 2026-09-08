package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ericovis/rasputin/internal/events"
)

var testSteps = []Step{
	{ID: events.StepPrepare, Title: "prepare the image", Estimate: 30 * time.Second},
	{ID: events.StepBake, Title: "bake the golden", Nodes: []string{"rasputin001"}, Estimate: 16 * time.Minute},
	{ID: events.StepFlash, Title: "flash the cluster", Nodes: []string{"rasputin002", "rasputin003"}, Estimate: 7 * time.Minute},
}

// newTestModel is a model of a known size, so View() lines are predictable.
func newTestModel(t *testing.T, steps []Step) *model {
	t.Helper()
	m := newModel("rasputin sync · cluster rasputin", steps, func() {})
	m.start = time.Unix(0, 0)
	m.now = time.Unix(0, 0)
	feed(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

// feed applies messages the way the bubbletea loop would.
func feed(m *model, msgs ...tea.Msg) *model {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(*model)
	}
	return m
}

func at(sec int) time.Time { return time.Unix(int64(sec), 0) }

func ev(k events.Kind, step events.StepID, sec int) events.Event {
	return events.Event{Kind: k, Step: step, Time: at(sec)}
}

func TestViewRendersSkipReasonAndSummary(t *testing.T) {
	m := newTestModel(t, testSteps)

	e := ev(events.StepSkipped, events.StepPrepare, 2)
	e.Message = "image matches rasputin.yaml"
	done := ev(events.StepDone, events.StepBake, 9)
	done.Message = "golden 20260829T205033Z-307368"
	feed(m, eventMsg(e), eventMsg(ev(events.StepStarted, events.StepBake, 3)), eventMsg(done))

	view := m.View()
	for _, want := range []string{
		"rasputin sync · cluster rasputin",
		"prepare",
		"skipped · image matches rasputin.yaml",
		"golden 20260829T205033Z-307368",
		"q / ctrl-c aborts",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View() is missing %q:\n%s", want, view)
		}
	}
	// A skipped step must not still claim it is going to do something.
	if strings.Contains(view, "prepare  prepare the image") {
		t.Errorf("skipped step still shows its title:\n%s", view)
	}
}

func TestPlannedSkipShowsItsReasonBeforeTheEvent(t *testing.T) {
	steps := []Step{{ID: events.StepAdopt, Skip: true, Reason: "all nodes adopted", Estimate: time.Minute}}
	m := newTestModel(t, steps)
	if want := "skipped · all nodes adopted"; !strings.Contains(m.View(), want) {
		t.Errorf("View() is missing %q:\n%s", want, m.View())
	}
}

func TestTransferMovesTheNodeBarAndTheOverallPercentage(t *testing.T) {
	m := newTestModel(t, testSteps)
	feed(m,
		eventMsg(ev(events.StepSkipped, events.StepPrepare, 1)),
		eventMsg(ev(events.StepSkipped, events.StepBake, 1)),
		eventMsg(ev(events.StepStarted, events.StepFlash, 2)),
	)
	before := m.lastFrac

	xfer := events.Event{Kind: events.Transfer, Step: events.StepFlash, Node: "rasputin002",
		Bytes: 2 << 30, Total: 4 << 30, Rate: 9.8e6, Time: at(10)}
	feed(m, eventMsg(xfer))

	view := m.View()
	if !strings.Contains(view, "2.0 GiB/4.0 GiB") {
		t.Errorf("node line is missing its byte counts:\n%s", view)
	}
	if !strings.Contains(view, "9.8 MB/s") {
		t.Errorf("node line is missing its rate:\n%s", view)
	}
	if !strings.Contains(view, "█") {
		t.Errorf("no bar was drawn:\n%s", view)
	}
	if m.lastFrac <= before {
		t.Errorf("overall progress did not move: %v -> %v", before, m.lastFrac)
	}
	// One node of two at half: the flash step is a quarter done, and flash is
	// the only step left to run.
	if got := m.lastFrac; got < 0.2 || got > 0.3 {
		t.Errorf("overall fraction = %.3f, want about 0.25", got)
	}
	// A Transfer is animation, not news: it must not fill the log panel.
	for _, line := range m.log {
		if strings.Contains(line, "MiB") || strings.Contains(line, "GiB") {
			t.Errorf("a Transfer was logged: %q", line)
		}
	}
}

func TestOverallProgressNeverGoesBackwards(t *testing.T) {
	m := newTestModel(t, testSteps)
	msgs := []tea.Msg{
		eventMsg(ev(events.StepStarted, events.StepPrepare, 1)),
		tickMsg(at(10)),
		eventMsg(ev(events.StepDone, events.StepPrepare, 20)),
		eventMsg(ev(events.StepStarted, events.StepBake, 21)),
		tickMsg(at(200)),
		eventMsg(ev(events.StepDone, events.StepBake, 900)),
		eventMsg(ev(events.StepStarted, events.StepFlash, 901)),
		tickMsg(at(1000)),
		eventMsg(ev(events.StepSkipped, events.StepFlash, 1100)),
	}
	last := 0.0
	for i, msg := range msgs {
		feed(m, msg)
		if m.lastFrac < last {
			t.Fatalf("progress went backwards at msg %d: %.3f -> %.3f", i, last, m.lastFrac)
		}
		last = m.lastFrac
	}
	if last < 0.99 {
		t.Errorf("finished run is only %.0f%% done", last*100)
	}
}

func TestPhaseUpdatesTheNodeLineAndTheLog(t *testing.T) {
	m := newTestModel(t, testSteps)
	phase := events.Event{Kind: events.Phase, Step: events.StepBake, Node: "rasputin001",
		Message: "capturing", Time: at(30)}
	feed(m, eventMsg(ev(events.StepStarted, events.StepBake, 5)), eventMsg(phase))

	view := m.View()
	if !strings.Contains(view, "rasputin001") || !strings.Contains(view, "capturing") {
		t.Errorf("node phase is not on screen:\n%s", view)
	}
	if !strings.Contains(view, at(30).Format("15:04:05")+" rasputin001: capturing") {
		t.Errorf("phase is not in the log panel:\n%s", view)
	}
}

func TestStepFailedIsVisibleAndKeepsTheError(t *testing.T) {
	m := newTestModel(t, testSteps)
	fail := ev(events.StepFailed, events.StepBake, 40)
	fail.Message = "capture timed out"
	feed(m, eventMsg(ev(events.StepStarted, events.StepBake, 5)), eventMsg(fail))

	view := m.View()
	if !strings.Contains(view, "failed · capture timed out") {
		t.Errorf("failure is not on the step line:\n%s", view)
	}
	if !strings.Contains(view, "✗") {
		t.Errorf("failed step is missing its glyph:\n%s", view)
	}
	if m.err == nil || !strings.Contains(m.err.Error(), "capture timed out") {
		t.Errorf("model error = %v, want it to carry the failure", m.err)
	}
}

func TestQuitKeyCancelsTheRun(t *testing.T) {
	cancelled := false
	m := newModel("rasputin sync", testSteps, func() { cancelled = true })
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if !cancelled {
		t.Fatal("q did not cancel the run context")
	}
	if cmd == nil {
		t.Fatal("q did not quit the program")
	}
	if !strings.Contains(m.View(), "aborting") {
		t.Errorf("View() does not say it is aborting:\n%s", m.View())
	}
}

func TestNarrowTerminalStaysWithinItsWidth(t *testing.T) {
	m := newTestModel(t, testSteps)
	feed(m, tea.WindowSizeMsg{Width: 80, Height: 24},
		eventMsg(ev(events.StepStarted, events.StepFlash, 1)),
		eventMsg(eventWithMessage(events.Log, events.StepFlash, 2,
			"rasputin002: arming a reflash from http://192.168.0.10:8080/golden/20260829T205033Z-307368.img.zst")),
		eventMsg(events.Event{Kind: events.Transfer, Step: events.StepFlash, Node: "rasputin002",
			Bytes: 1 << 30, Total: 4 << 30, Rate: 4.3e6, Time: at(3)}),
	)
	for i, line := range strings.Split(strings.TrimRight(m.View(), "\n"), "\n") {
		if w := lipglossWidth(line); w > 80 {
			t.Errorf("line %d is %d columns wide at 80: %q", i, w, line)
		}
	}
}

func eventWithMessage(k events.Kind, step events.StepID, sec int, msg string) events.Event {
	e := ev(k, step, sec)
	e.Message = msg
	return e
}

// TestMultiLineFailureKeepsTheFrameIntact: a flash failure reports one line
// per node. lipgloss.Width measures the widest line of a multi-line string,
// so nothing truncates it and the raw newlines land inside the log box, the
// step list and the footer.
func TestMultiLineFailureKeepsTheFrameIntact(t *testing.T) {
	const failure = "3 of 3 node(s) failed to flash:\n" +
		"rasputin002: stuck in the recovery agent\n" +
		"rasputin003: stuck in the recovery agent\n" +
		"rasputin004: stuck in the recovery agent"

	render := func(msg string) (string, *model) {
		m := newTestModel(t, testSteps)
		e := ev(events.StepFailed, events.StepFlash, 12)
		e.Message = msg
		return feed(m, eventMsg(e)).View(), m
	}
	view, m := render(failure)
	oneLine, _ := render(flatten(failure))

	if got, want := len(strings.Split(view, "\n")), len(strings.Split(oneLine, "\n")); got != want {
		t.Errorf("a multi-line failure draws %d rows where one line draws %d:\n%s", got, want, view)
	}
	for _, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > 100 {
			t.Errorf("line %q is %d columns wide, past the terminal's 100", line, w)
		}
	}
	// The whole text still reaches the log the panel tails, in one entry.
	if n := len(m.log); n != 1 {
		t.Fatalf("the failure became %d log entries, want 1", n)
	}
	if !strings.Contains(m.log[0], "rasputin004") {
		t.Errorf("the log entry lost the last node: %q", m.log[0])
	}
}

func TestFlatten(t *testing.T) {
	cases := []struct{ in, want string }{
		{"one line", "one line"},
		{"first\nsecond", "first · second"},
		{"trailing\n", "trailing"},
		{"crlf\r\nnext", "crlf · next"},
	}
	for _, tc := range cases {
		if got := flatten(tc.in); got != tc.want {
			t.Errorf("flatten(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTransferPromotesAWaitingNode(t *testing.T) {
	m := newTestModel(t, testSteps)
	feed(m, eventMsg(ev(events.StepStarted, events.StepFlash, 1)))
	tr := ev(events.Transfer, events.StepFlash, 2)
	tr.Node, tr.Bytes, tr.Total, tr.Rate = "rasputin002", 10<<20, 100<<20, 4e6
	feed(m, eventMsg(tr))

	var line string
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.Contains(l, "rasputin002") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no line for rasputin002 in:\n%s", m.View())
	}
	if strings.Contains(line, "waiting") || !strings.Contains(line, "downloading the image") {
		t.Errorf("a node with bytes in flight should read downloading, got %q", line)
	}
}
