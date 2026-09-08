package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNothingPrintsPastTheOutputLayer: in JSON mode stdout is JSON only, so
// no command may write to it directly. Everything goes through output.
func TestNothingPrintsPastTheOutputLayer(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "output.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "fmt.Print") || strings.Contains(line, "os.Stdout") {
				t.Errorf("%s:%d writes to stdout directly; use output.printf/logf/result: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// testOutput is an output whose stdout is buf and whose stderr is thrown
// away, so tests see exactly what a program piping the command would see.
func testOutput(buf *bytes.Buffer) *output {
	return &output{w: buf, errw: &bytes.Buffer{}}
}

func jsonOutput(buf *bytes.Buffer) *output {
	o := testOutput(buf)
	o.json = true
	return o
}

// lines parses a JSON stream, failing the test on any line that is not one
// object: in JSON mode nothing else may reach stdout.
func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line %d is not a JSON object: %q (%v)\nwhole output:\n%s", i+1, line, err, buf.String())
		}
		if _, ok := m["type"]; !ok {
			t.Errorf("line %d has no \"type\": %s", i+1, line)
		}
		out = append(out, m)
	}
	return out
}

// result returns the final object, checking it is a result.
func result(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	ls := lines(t, buf)
	last := ls[len(ls)-1]
	if last["type"] != "result" {
		t.Fatalf("the last line is %v, want a result:\n%s", last, buf.String())
	}
	return last
}

// TestEveryCommandAcceptsJSON is the contract the manual promises: -json is
// a flag of every command. Each command parses its flags before it does
// anything, so -json -h proves the flag is defined without touching a node.
func TestEveryCommandAcceptsJSON(t *testing.T) {
	cfg := upConfig(t)
	for _, c := range commands {
		var buf bytes.Buffer
		err := c.run(cfg, testOutput(&buf), []string{"-json", "-h"})
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s -json -h: err = %v, want flag.ErrHelp (is -json defined?)", c.name, err)
		}
		if buf.Len() > 0 {
			t.Errorf("%s -h wrote usage to stdout, want stderr:\n%s", c.name, buf.String())
		}
	}
}

func TestHasJSONFlag(t *testing.T) {
	cases := map[bool][][]string{
		true:  {{"-json"}, {"--json"}, {"-yes", "-json"}, {"-json=true", "all"}, {"-json", "rasputin002"}},
		false: {{}, {"-yes"}, {"json"}, {"-json=false"}, {"--", "-json"}, {"-log", "json"}},
	}
	for want, argss := range cases {
		for _, args := range argss {
			if got := hasJSONFlag(args); got != want {
				t.Errorf("hasJSONFlag(%q) = %v, want %v", args, got, want)
			}
		}
	}
}

func TestResultEnvelope(t *testing.T) {
	t.Run("success with fields", func(t *testing.T) {
		var buf bytes.Buffer
		o := jsonOutput(&buf)
		if err := o.result("status", struct {
			N int `json:"n"`
		}{3}, nil); err != nil {
			t.Fatal(err)
		}
		got := result(t, &buf)
		if got["command"] != "status" || got["ok"] != true || got["n"] != float64(3) {
			t.Errorf("result = %v", got)
		}
		if _, has := got["error"]; has {
			t.Errorf("a success carries an error field: %v", got)
		}
	})
	t.Run("failure without fields", func(t *testing.T) {
		var buf bytes.Buffer
		o := jsonOutput(&buf)
		want := errors.New("boom")
		if err := o.result("flash", nil, want); err != want {
			t.Fatalf("result returned %v, want the error unchanged", err)
		}
		got := result(t, &buf)
		if got["ok"] != false || got["error"] != "boom" || got["command"] != "flash" {
			t.Errorf("result = %v", got)
		}
	})
	t.Run("only one result is ever written", func(t *testing.T) {
		var buf bytes.Buffer
		o := jsonOutput(&buf)
		o.result("x", nil, nil)
		o.result("x", nil, errors.New("late"))
		if n := len(lines(t, &buf)); n != 1 {
			t.Errorf("%d result lines, want 1:\n%s", n, buf.String())
		}
	})
	t.Run("text mode writes nothing", func(t *testing.T) {
		var buf bytes.Buffer
		o := testOutput(&buf)
		o.result("x", map[string]int{"n": 1}, nil)
		if buf.Len() != 0 {
			t.Errorf("text mode wrote %q", buf.String())
		}
	})
	t.Run("printf is silent and logf is an object in JSON mode", func(t *testing.T) {
		var buf bytes.Buffer
		o := jsonOutput(&buf)
		o.printf("a table\n")
		o.logf("served %s", "x")
		ls := lines(t, &buf)
		if len(ls) != 1 || ls[0]["type"] != "log" || ls[0]["message"] != "served x" {
			t.Errorf("lines = %v", ls)
		}
	})
}

// TestRunReportsAnUnloadableConfigInJSON: -json after the command name must
// shape even the errors that happen before the command runs.
func TestRunReportsAnUnloadableConfigInJSON(t *testing.T) {
	var buf bytes.Buffer
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	err := run([]string{"-c", missing, "status", "-json"}, testOutput(&buf))
	if err == nil {
		t.Fatal("a missing config loaded")
	}
	got := result(t, &buf)
	if got["ok"] != false || got["command"] != "status" || got["error"] == "" {
		t.Errorf("result = %v", got)
	}

	buf.Reset()
	err = run([]string{"-json", "bogus"}, testOutput(&buf))
	if err == nil {
		t.Fatal("an unknown command ran")
	}
	if got := result(t, &buf); got["command"] != "bogus" || got["ok"] != false {
		t.Errorf("result = %v", got)
	}
}

func TestInitJSON(t *testing.T) {
	var buf bytes.Buffer
	path := filepath.Join(t.TempDir(), "rasputin.yaml")
	if err := initConfig(jsonOutput(&buf), path, []string{"-node", "pi1=b8:27:eb:01:02:03", "-builder", "pi1"}); err != nil {
		t.Fatal(err)
	}
	got := result(t, &buf)
	if got["ok"] != true || got["path"] != path || got["builder"] != "pi1" || got["placeholder"] != false {
		t.Errorf("result = %v", got)
	}
	nodes, _ := got["nodes"].([]any)
	if len(nodes) != 1 {
		t.Errorf("nodes = %v", got["nodes"])
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config not written: %v", err)
	}
}

func TestManual(t *testing.T) {
	cfg := upConfig(t)
	for _, c := range commands {
		if !strings.Contains(manualText, "### `"+c.name) {
			t.Errorf("MANUAL.md has no section for %q", c.name)
		}
	}
	for _, want := range []string{"-json", `"type":"result"`, "-plan", "-yes", "WILL", "wipes"} {
		if !strings.Contains(manualText, want) {
			t.Errorf("MANUAL.md does not mention %q", want)
		}
	}

	var buf bytes.Buffer
	if err := runManual(cfg, testOutput(&buf), nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != manualText {
		t.Error("manual printed something other than MANUAL.md")
	}

	buf.Reset()
	if err := runManual(cfg, jsonOutput(&buf), nil); err != nil {
		t.Fatal(err)
	}
	got := result(t, &buf)
	if got["manual"] != manualText {
		t.Error("manual -json does not carry the text")
	}
	cmds, _ := got["commands"].([]any)
	if len(cmds) != len(commands) {
		t.Errorf("manual -json lists %d commands, want %d", len(cmds), len(commands))
	}
}

// TestSyncJSONStream is the machine contract of the one command that
// streams: plan first, events, result last, nothing else on stdout.
func TestSyncJSONStream(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil
	f.nodeBuild = "build-0"
	opts := upOpts()
	opts.Yes = true

	var buf bytes.Buffer
	if err := executeSync(context.Background(), f.deps(), opts, syncUI{Out: jsonOutput(&buf)}); err != nil {
		t.Fatalf("sync -json -yes: %v", err)
	}
	ls := lines(t, &buf)
	if ls[0]["type"] != "plan" {
		t.Fatalf("first line is %v, want the plan", ls[0])
	}
	plan := ls[0]
	if plan["needs_confirmation"] != true {
		t.Errorf("a plan that bakes says needs_confirmation=%v", plan["needs_confirmation"])
	}
	wipes, _ := plan["wipes"].([]any)
	if len(wipes) == 0 {
		t.Errorf("plan lists no wipes although it bakes: %v", plan)
	}
	var events int
	for _, l := range ls[1 : len(ls)-1] {
		if l["type"] != "event" {
			t.Errorf("middle line is not an event: %v", l)
		}
		events++
	}
	if events == 0 {
		t.Error("no events were streamed")
	}
	res := ls[len(ls)-1]
	if res["type"] != "result" || res["ok"] != true || res["aborted"] != false {
		t.Errorf("result = %v", res)
	}
	steps, _ := res["steps"].([]any)
	if len(steps) == 0 {
		t.Errorf("result has no steps: %v", res)
	}
	status, _ := res["status"].([]any)
	if len(status) != 4 {
		t.Errorf("result status has %d nodes, want 4", len(status))
	}
}

// TestSyncJSONNeverPrompts: a program cannot answer y, so a destructive plan
// without -yes must fail, after emitting the plan and a result.
func TestSyncJSONNeverPrompts(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil

	var buf bytes.Buffer
	err := executeSync(context.Background(), f.deps(), upOpts(), syncUI{Out: jsonOutput(&buf)})
	if err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("error = %v, want it to demand -yes", err)
	}
	if f.bakes != 0 {
		t.Error("the bake ran without confirmation")
	}
	ls := lines(t, &buf)
	if ls[0]["type"] != "plan" {
		t.Errorf("first line = %v, want the plan", ls[0])
	}
	// The result is added by main, which this test bypasses; the command
	// itself leaves the stream with the plan only.
	if len(ls) != 1 {
		t.Errorf("stream has %d lines, want just the plan:\n%s", len(ls), buf.String())
	}
}

// TestSyncPlanOnly: -plan is the read-only way to see what sync would do.
func TestSyncPlanOnly(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil

	var buf bytes.Buffer
	ui := syncUI{Out: jsonOutput(&buf), PlanOnly: true, Confirm: func() (bool, error) {
		t.Error("-plan asked for confirmation")
		return false, nil
	}}
	if err := executeSync(context.Background(), f.deps(), upOpts(), ui); err != nil {
		t.Fatalf("sync -plan: %v", err)
	}
	if f.bakes != 0 || len(f.flashed) != 0 {
		t.Error("-plan touched the cluster")
	}
	ls := lines(t, &buf)
	if len(ls) != 2 || ls[0]["type"] != "plan" || ls[1]["type"] != "result" || ls[1]["plan_only"] != true {
		t.Errorf("stream = %v", ls)
	}

	// And in text mode, the summary and nothing else.
	buf.Reset()
	if err := executeSync(context.Background(), f.deps(), upOpts(), syncUI{Out: testOutput(&buf), Plain: true, PlanOnly: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "WILL WIPE") || strings.Contains(buf.String(), "STEP      RESULT") {
		t.Errorf("text -plan output:\n%s", buf.String())
	}
}

// TestSyncJSONFailedStep: the result names the failed step and the error.
func TestSyncJSONFailedStep(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil
	f.bakeErr = errors.New("the builder never came back")
	opts := upOpts()
	opts.Yes = true

	var buf bytes.Buffer
	err := executeSync(context.Background(), f.deps(), opts, syncUI{Out: jsonOutput(&buf)})
	if err == nil {
		t.Fatal("a failed bake returned no error")
	}
	res := result(t, &buf)
	if res["ok"] != false || !strings.Contains(res["error"].(string), "never came back") {
		t.Errorf("result = %v", res)
	}
	steps := res["steps"].([]any)
	var sawFail bool
	for _, s := range steps {
		m := s.(map[string]any)
		if m["id"] == "bake" && m["error"] != nil {
			sawFail = true
		}
	}
	if !sawFail {
		t.Errorf("no bake step with an error in %v", steps)
	}
	if st := res["status"].([]any); len(st) != 0 {
		t.Errorf("status after a failed run is %v, want empty", st)
	}
}
