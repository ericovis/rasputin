package events

import (
	"bytes"
	"strings"
	"testing"
)

func TestLogfTagsNodePrefixedLines(t *testing.T) {
	var rec Recorder
	logf := Logf(&rec, StepFlash, []string{"rasputin001", "rasputin002"})
	logf("%s: arming a reflash", "rasputin002")
	logf("serving golden build %s", "abc")

	got := rec.Of(Log)
	if len(got) != 2 {
		t.Fatalf("got %d log events, want 2", len(got))
	}
	if got[0].Node != "rasputin002" || got[0].Message != "rasputin002: arming a reflash" {
		t.Errorf("first event = %+v, want node rasputin002 with the prefix kept", got[0])
	}
	if got[1].Node != "" {
		t.Errorf("second event tagged %q, want no node", got[1].Node)
	}
	if got[0].Step != StepFlash || got[0].Time.IsZero() {
		t.Errorf("event missing step or timestamp: %+v", got[0])
	}
}

func TestPlainRendersStepsAndHidesTransfersByDefault(t *testing.T) {
	var buf bytes.Buffer
	p := NewPlain(&buf)
	p.Emit(Event{Kind: StepStarted, Step: StepBake, Message: "on rasputin001"})
	p.Emit(Event{Kind: Transfer, Step: StepBake, Node: "rasputin001", Bytes: 50 << 20, Total: 100 << 20, Rate: 4.3e6})
	p.Emit(Event{Kind: StepSkipped, Step: StepPrepare, Message: "config unchanged"})
	p.Emit(Event{Kind: StepFailed, Step: StepFlash, Message: "boom"})

	out := buf.String()
	for _, want := range []string{"==> bake — on rasputin001", "--- prepare: skipped — config unchanged", "!!! flash: FAILED — boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MiB") {
		t.Errorf("transfer printed although ShowTransfers is off:\n%s", out)
	}

	p.ShowTransfers = true
	buf.Reset()
	p.Emit(Event{Kind: Transfer, Node: "rasputin001", Bytes: 50 << 20, Total: 100 << 20, Rate: 4.3e6})
	if got := strings.TrimSpace(buf.String()); got != "rasputin001: 50/100 MiB (50%, 4.3 MB/s)" {
		t.Errorf("transfer line = %q", got)
	}
}

func TestTeeFansOut(t *testing.T) {
	var a, b Recorder
	Tee(&a, nil, &b).Emit(Event{Kind: Phase, Step: StepBake, Message: "sealing"})
	if len(a.Events()) != 1 || len(b.Events()) != 1 {
		t.Fatalf("tee delivered %d/%d, want 1/1", len(a.Events()), len(b.Events()))
	}
	if got := Format(a.Events()[0]); got != "bake: sealing" {
		t.Errorf("Format = %q", got)
	}
}
