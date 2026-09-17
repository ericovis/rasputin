package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ericovis/rasputin/internal/cluster"
)

// TestStatusJSONCarriesTheOverlay: `overlay` is what an agent driving the
// tool reads to know whether a node can be reset at all, and it is one
// assignment away from silently reporting false for a healthy cluster —
// which looks exactly like the agent's real overlay-fallback path.
func TestStatusJSONCarriesTheOverlay(t *testing.T) {
	var buf bytes.Buffer
	out := jsonOutput(&buf)
	rows := []cluster.Status{
		{Name: "rasputin001", MAC: "b8:27:eb:01:02:03", IP: "192.168.0.71", Reachable: true,
			SSHUser: "berry", Hostname: "rasputin001", BuildID: "build-1",
			Provisioned: true, Adopted: true, Overlay: true},
		{Name: "rasputin002", MAC: "b8:27:eb:04:05:06", Reachable: false,
			Err: errors.New("no route to host")},
	}
	if err := out.result("status", struct {
		Nodes []statusJSON `json:"nodes"`
	}{toStatusJSON(rows)}, nil); err != nil {
		t.Fatalf("result: %v", err)
	}

	nodes, _ := result(t, &buf)["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %v, want two rows", nodes)
	}
	first, _ := nodes[0].(map[string]any)
	if first["name"] != "rasputin001" || first["overlay"] != true {
		t.Errorf("first node = %v, want rasputin001 on an overlay", first)
	}
	// The field is not omitempty on purpose: a node that answered and is not
	// on an overlay has to say so, not go missing.
	second, _ := nodes[1].(map[string]any)
	if _, ok := second["overlay"]; !ok || second["overlay"] != false {
		t.Errorf("second node = %v, want overlay reported as false", second)
	}
}
