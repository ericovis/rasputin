package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ericovis/rasputin/internal/events"
)

// eventMsg carries one orchestration event into the update loop.
type eventMsg events.Event

// tickMsg redraws the clock and the bars between events.
type tickMsg time.Time

// doneMsg says run returned.
type doneMsg struct{ err error }

// tickEvery is fast enough to look alive, slow enough to stay quiet.
const tickEvery = 200 * time.Millisecond

// maxLog is how much scrollback the log panel keeps; only what fits is drawn.
const maxLog = 200

type stepStatus int

const (
	statusPending stepStatus = iota
	statusRunning
	statusSkipped
	statusDone
	statusFailed
)

// nodeState is one node's line under a running step.
type nodeState struct {
	name  string
	phase string
	// bytes/total/rate come from Transfer events; total <= 0 means unknown.
	bytes int64
	total int64
	rate  float64
	state stepStatus
	bar   progress.Model
}

// stepState is one row of the step list.
type stepState struct {
	Step
	status  stepStatus
	message string // done summary, skip reason or failure
	phase   string // last step-wide Phase message
	started time.Time
	ended   time.Time
	nodes   []*nodeState
}

func (s *stepState) node(name string) *nodeState {
	for _, n := range s.nodes {
		if n.name == name {
			return n
		}
	}
	n := &nodeState{name: name, phase: "waiting", bar: newBar()}
	s.nodes = append(s.nodes, n)
	return n
}

// model is the whole screen. It owns no I/O: every field moves in Update, so
// View is a pure function of state and the tests can drive it with messages.
type model struct {
	title  string
	steps  []*stepState
	log    []string
	width  int
	height int

	spin    spinner.Model
	overall progress.Model

	start     time.Time
	now       time.Time
	lastFrac  float64
	remaining time.Duration

	cancel  context.CancelFunc
	aborted bool
	err     error
	done    bool
}

func newModel(title string, steps []Step, cancel context.CancelFunc) *model {
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = runningStyle
	m := &model{
		title:   title,
		width:   80,
		height:  24,
		spin:    sp,
		overall: newBar(),
		start:   time.Now(),
		now:     time.Now(),
		cancel:  cancel,
	}
	for _, s := range steps {
		st := &stepState{Step: s}
		for _, n := range s.Nodes {
			st.nodes = append(st.nodes, &nodeState{name: n, phase: "waiting", bar: newBar()})
		}
		m.steps = append(m.steps, st)
	}
	return m
}

func newBar() progress.Model {
	return progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage())
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.abort()
			cmd = tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		m.now = time.Time(msg)
		cmd = tickCmd()
	case spinner.TickMsg:
		m.spin, cmd = m.spin.Update(msg)
	case eventMsg:
		m.apply(events.Event(msg))
	case doneMsg:
		m.done = true
		if msg.err != nil && m.err == nil {
			m.err = msg.err
		}
		cmd = tea.Quit
	}
	m.refresh()
	return m, cmd
}

// refresh recomputes the overall bar. It lives in Update so View stays a pure
// read of the model, and so the bar can be held monotonic across redraws.
func (m *model) refresh() {
	frac, left := overallFraction(m.stepProgress())
	if frac > m.lastFrac {
		m.lastFrac = frac
	}
	m.remaining = left
}

// stepProgress projects the steps onto the pure inputs overallFraction needs.
func (m *model) stepProgress() []stepProgress {
	out := make([]stepProgress, 0, len(m.steps))
	for _, s := range m.steps {
		p := stepProgress{Estimate: s.Estimate, Status: s.status, Fraction: -1}
		if s.status == statusPending && s.Skip {
			// The plan already knows this one will be skipped; do not let it
			// inflate the denominator and stall the bar.
			p.Status = statusSkipped
		}
		if s.status == statusRunning {
			if !s.started.IsZero() {
				p.Elapsed = m.now.Sub(s.started)
			}
			p.Fraction = s.byteFraction()
		}
		out = append(out, p)
	}
	return out
}

// byteFraction averages the step's nodes: a finished node counts as whole, a
// downloading one as its share, one that has not started as nothing. It is -1
// when no node has said anything yet, and the clock takes over.
func (s *stepState) byteFraction() float64 {
	if len(s.nodes) == 0 {
		return -1
	}
	var sum float64
	known := false
	for _, n := range s.nodes {
		switch {
		case n.state == statusDone || n.state == statusSkipped:
			sum, known = sum+1, true
		case n.total > 0:
			f := float64(n.bytes) / float64(n.total)
			if f > 1 {
				f = 1
			}
			sum, known = sum+f, true
		}
	}
	if !known {
		return -1
	}
	return sum / float64(len(s.nodes))
}

// abort cancels the run's context. The nodes are left in the recovery agent,
// which is a safe place to be: nothing is half-written by stopping here.
func (m *model) abort() {
	if m.aborted {
		return
	}
	m.aborted = true
	if m.cancel != nil {
		m.cancel()
	}
}

// apply folds one event into the model.
func (m *model) apply(e events.Event) {
	// Flattened once, here, because every consumer below draws inside a
	// bordered box: a flash failure reports one line per node, and a raw
	// newline in the log panel or a step's detail tears the frame apart.
	e.Message = flatten(e.Message)
	if e.Time.IsZero() {
		e.Time = m.now
	}
	if e.Time.After(m.now) {
		m.now = e.Time
	}
	s := m.step(e.Step)
	switch e.Kind {
	case events.StepStarted:
		if s != nil {
			s.status = statusRunning
			s.started = e.Time
			s.phase = e.Message
		}
		m.logf(e)
	case events.StepSkipped:
		if s != nil {
			s.status = statusSkipped
			s.message = e.Message
			s.started, s.ended = e.Time, e.Time
		}
		m.logf(e)
	case events.StepDone:
		if s != nil {
			s.status = statusDone
			s.message = e.Message
			s.ended = e.Time
			if s.started.IsZero() {
				s.started = e.Time
			}
		}
		m.logf(e)
	case events.StepFailed:
		if s != nil {
			s.status = statusFailed
			s.message = e.Message
			s.ended = e.Time
			if s.started.IsZero() {
				s.started = e.Time
			}
		}
		if m.err == nil && e.Message != "" {
			m.err = fmt.Errorf("%s: %s", e.Step, e.Message)
		}
		m.logf(e)
	case events.Phase:
		if s != nil {
			if e.Node == "" {
				s.phase = e.Message
			} else {
				n := s.node(e.Node)
				n.phase = e.Message
				n.state = nodeStatusOf(e.Message)
			}
		}
		m.logf(e)
	case events.Log:
		m.logf(e)
	case events.Transfer:
		// Transfers animate the bars; logging them would drown the panel.
		if s == nil || e.Node == "" {
			return
		}
		n := s.node(e.Node)
		n.bytes, n.total, n.rate = e.Bytes, e.Total, e.Rate
		if n.state == statusPending {
			n.state = statusRunning
		}
		// Bytes moving means the node is downloading, whatever Phase said
		// so far; a line that reads "waiting" beside a growing bar lies.
		if n.phase == "waiting" {
			n.phase = "downloading the image"
		}
	}
}

// nodeStatusOf reads a phase message for the words that mean the node is
// finished with this step. Anything else is just work in progress.
func nodeStatusOf(msg string) stepStatus {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "fail"), strings.Contains(l, "error"):
		return statusFailed
	case strings.Contains(l, "skip"):
		return statusSkipped
	case strings.Contains(l, "done"), strings.Contains(l, "healthy"), strings.Contains(l, "ok"):
		return statusDone
	}
	return statusRunning
}

func (m *model) step(id events.StepID) *stepState {
	for _, s := range m.steps {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// flatten turns a multi-line message into one line. The whole text is in
// out/sync.log and in the result table the caller prints afterwards.
func flatten(s string) string {
	if !strings.ContainsAny(s, "\n\r") {
		return s
	}
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	for i, f := range fields {
		fields[i] = strings.TrimSpace(f)
	}
	return strings.Join(fields, " · ")
}

func (m *model) logf(e events.Event) {
	text := events.Format(e)
	if e.Kind == events.Log {
		text = e.Message
	}
	if text == "" {
		return
	}
	m.log = append(m.log, e.Time.Format("15:04:05")+" "+text)
	if len(m.log) > maxLog {
		m.log = m.log[len(m.log)-maxLog:]
	}
}
