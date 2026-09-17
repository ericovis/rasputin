package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// TestResetNeedsANodeArgument: which nodes to reset is the whole command, and
// guessing "all" would wipe four nodes nobody named.
func TestResetNeedsANodeArgument(t *testing.T) {
	var buf bytes.Buffer
	err := runReset(upConfig(t), testOutput(&buf), []string{"-yes"})
	if err == nil {
		t.Fatal("reset with no node argument was accepted")
	}
	if !strings.Contains(err.Error(), "no node given") {
		t.Errorf("error = %v, want it to ask for a node", err)
	}
	if buf.Len() > 0 {
		t.Errorf("usage reached stdout:\n%s", buf.String())
	}
}

func TestResetRejectsAnUnknownNode(t *testing.T) {
	var buf bytes.Buffer
	err := runReset(upConfig(t), testOutput(&buf), []string{"-yes", "rasputin009"})
	if err == nil {
		t.Fatal("reset accepted a node that is not in the config")
	}
	if !strings.Contains(err.Error(), "rasputin009") {
		t.Errorf("error = %v, want it to name the unknown node", err)
	}
}

// TestResetInJSONModeDemandsYes: a program cannot answer a prompt, so it has
// to say -yes to mean it — and the refusal has to say what it would agree to.
func TestResetInJSONModeDemandsYes(t *testing.T) {
	var buf bytes.Buffer
	targets := []config.Node{{Name: "rasputin002"}, {Name: "rasputin003"}}
	ok, err := confirmReset(jsonOutput(&buf), nil, targets)
	if ok || err == nil {
		t.Fatalf("confirmReset in JSON mode = (%v, %v), want a refusal", ok, err)
	}
	for _, want := range []string{"-yes", "rasputin002 rasputin003", "wipes everything"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if buf.Len() > 0 {
		t.Errorf("the refusal wrote to stdout:\n%s", buf.String())
	}
}

// TestResetReportShape is the machine contract of the command: the cluster
// call is the only part hardware is needed for, and everything either side of
// it is this.
func TestResetReportShape(t *testing.T) {
	results := []cluster.ResetResult{
		{Node: "rasputin002", Overlay: true, Duration: 92 * time.Second},
		{Node: "rasputin003", Err: errors.New("rasputin003 has no writable layer " +
			"(its golden predates overlay support): flash it first")},
	}

	var buf bytes.Buffer
	out := jsonOutput(&buf)
	report := resetReport(out, results)
	if report.Reset != 1 || report.Failed != 1 {
		t.Errorf("report = %+v, want one reset and one failure", report)
	}
	if err := out.result("reset", report, errors.New("1 of 2 node(s) failed to reset")); err == nil {
		t.Fatal("result returned no error")
	}

	got := result(t, &buf)
	if got["ok"] != false || got["reset"] != float64(1) || got["failed"] != float64(1) {
		t.Errorf("result = %v", got)
	}
	nodes, _ := got["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %v", got["nodes"])
	}
	first, _ := nodes[0].(map[string]any)
	if first["node"] != "rasputin002" || first["ok"] != true ||
		first["overlay"] != true || first["duration_seconds"] != float64(92) {
		t.Errorf("first node = %v", first)
	}
	second, _ := nodes[1].(map[string]any)
	if second["ok"] != false || second["overlay"] != false ||
		!strings.Contains(second["error"].(string), "flash it first") {
		t.Errorf("second node = %v", second)
	}
}

// TestResetPrintsATableInTextMode: the same outcomes, for a person.
func TestResetPrintsATableInTextMode(t *testing.T) {
	var buf bytes.Buffer
	resetReport(testOutput(&buf), []cluster.ResetResult{
		{Node: "rasputin002", Overlay: true, Duration: 92 * time.Second},
		{Node: "rasputin003", Err: errors.New("no writable layer")},
	})
	for _, want := range []string{"NODE", "rasputin002", "PASS", "1m32s", "rasputin003", "FAIL", "no writable layer"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("table is missing %q:\n%s", want, buf.String())
		}
	}
}
