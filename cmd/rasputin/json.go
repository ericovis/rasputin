package main

// The JSON forms of what the commands report. They are views, not the
// domain types: an `error` value does not marshal usefully, a Duration
// marshals as nanoseconds, and a field name is a contract once a program
// depends on it. MANUAL.md documents every one of these; change both.

import (
	"strings"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/events"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/up"
)

// jsonEvent is one `sync` event on the stream.
type jsonEvent struct {
	Type string `json:"type"`
	events.Wire
}

func toJSONEvent(e events.Event) jsonEvent { return jsonEvent{Type: "event", Wire: events.ToWire(e)} }

// nodeJSON is a node as configured.
type nodeJSON struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

func toNodesJSON(nodes []config.Node) []nodeJSON {
	out := make([]nodeJSON, len(nodes))
	for i, n := range nodes {
		out[i] = nodeJSON{Name: n.Name, MAC: n.MAC}
	}
	return out
}

// statusJSON is one row of the health table. build_id is empty on a node
// running a stock OS; adopted says whether it boots through the recovery
// agent; error is set when reachable is false.
type statusJSON struct {
	Name        string `json:"name"`
	MAC         string `json:"mac"`
	IP          string `json:"ip,omitempty"`
	Reachable   bool   `json:"reachable"`
	SSHUser     string `json:"ssh_user,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	BuildID     string `json:"build_id,omitempty"`
	Uptime      string `json:"uptime,omitempty"`
	Provisioned bool   `json:"provisioned"`
	Adopted     bool   `json:"adopted"`
	Error       string `json:"error,omitempty"`
}

func toStatusJSON(rows []cluster.Status) []statusJSON {
	out := make([]statusJSON, len(rows))
	for i, r := range rows {
		out[i] = statusJSON{
			Name:        r.Name,
			MAC:         r.MAC,
			IP:          r.IP,
			Reachable:   r.Reachable,
			SSHUser:     r.SSHUser,
			Hostname:    trim(r.Hostname),
			BuildID:     r.BuildID,
			Uptime:      trim(r.Uptime),
			Provisioned: r.Provisioned,
			Adopted:     r.Adopted,
			Error:       errString(r.Err),
		}
	}
	return out
}

// imageJSON is an image the built-in HTTP server is offering.
type imageJSON struct {
	Name string `json:"name"`
	Path string `json:"path"`
	URL  string `json:"url"`
}

// checkJSON is one preflight check on a node about to be adopted.
type checkJSON struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type preflightJSON struct {
	User   string      `json:"user,omitempty"`
	Host   string      `json:"host,omitempty"`
	Checks []checkJSON `json:"checks"`
}

func toPreflightJSON(p nodes.Preflight) preflightJSON {
	out := preflightJSON{User: p.User, Host: p.Host, Checks: make([]checkJSON, len(p.Checks))}
	for i, c := range p.Checks {
		out.Checks[i] = checkJSON{Name: c.Name, OK: c.OK, Detail: c.Detail}
	}
	return out
}

// adoptNodeJSON is one node's adoption outcome.
type adoptNodeJSON struct {
	Node      string        `json:"node"`
	OK        bool          `json:"ok"`
	Rebooted  bool          `json:"rebooted"`
	Preflight preflightJSON `json:"preflight"`
	Error     string        `json:"error,omitempty"`
}

func toAdoptJSON(r cluster.AdoptResult) adoptNodeJSON {
	return adoptNodeJSON{
		Node:      r.Node,
		OK:        r.OK(),
		Rebooted:  r.Rebooted,
		Preflight: toPreflightJSON(r.Preflight),
		Error:     errString(r.Err),
	}
}

// dryrunNodeJSON is one node's rehearsal outcome; report is the
// reflash-dryrun.log the node left on its boot partition, verbatim.
type dryrunNodeJSON struct {
	Node   string `json:"node"`
	OK     bool   `json:"ok"`
	Report string `json:"report,omitempty"`
	Error  string `json:"error,omitempty"`
}

func toDryrunJSON(r cluster.DryrunResult) dryrunNodeJSON {
	return dryrunNodeJSON{Node: r.Node, OK: r.OK(), Report: r.Report, Error: errString(r.Err)}
}

// flashNodeJSON is one node's flash outcome. A skipped node already ran the
// golden build and is a success.
type flashNodeJSON struct {
	Node            string  `json:"node"`
	OK              bool    `json:"ok"`
	Skipped         bool    `json:"skipped"`
	BuildID         string  `json:"build_id,omitempty"`
	Hostname        string  `json:"hostname,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	Error           string  `json:"error,omitempty"`
}

func toFlashJSON(r cluster.FlashResult) flashNodeJSON {
	return flashNodeJSON{
		Node:            r.Node,
		OK:              r.OK(),
		Skipped:         r.Skipped,
		BuildID:         r.BuildID,
		Hostname:        trim(r.Hostname),
		DurationSeconds: seconds(r.Duration),
		Error:           errString(r.Err),
	}
}

// planJSON is what `sync` intends to do, emitted before anything runs.
type planJSON struct {
	Type              string         `json:"type"`
	Cluster           string         `json:"cluster"`
	NeedsConfirmation bool           `json:"needs_confirmation"`
	Wipes             []wipeJSON     `json:"wipes"`
	EstimateSeconds   float64        `json:"estimate_seconds"`
	Steps             []planStepJSON `json:"steps"`
	Status            []statusJSON   `json:"status"`
}

// wipeJSON names a node that loses its card, and the step that does it.
type wipeJSON struct {
	Node string `json:"node"`
	Step string `json:"step"`
}

type planStepJSON struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Skip            bool     `json:"skip"`
	Reason          string   `json:"reason,omitempty"`
	Nodes           []string `json:"nodes"`
	Wipes           []string `json:"wipes"`
	EstimateSeconds float64  `json:"estimate_seconds"`
}

func toPlanJSON(p *up.Plan) planJSON {
	out := planJSON{
		Type:              "plan",
		Cluster:           p.Cluster,
		NeedsConfirmation: p.NeedsConfirmation(),
		Wipes:             []wipeJSON{},
		EstimateSeconds:   seconds(p.Estimate()),
		Steps:             make([]planStepJSON, len(p.Steps)),
		Status:            toStatusJSON(p.Status),
	}
	for i, s := range p.Steps {
		out.Steps[i] = planStepJSON{
			ID:              string(s.ID),
			Title:           s.Title,
			Skip:            s.Skip,
			Reason:          s.Reason,
			Nodes:           nonNil(s.Nodes),
			Wipes:           nonNil(s.Wipes),
			EstimateSeconds: seconds(s.Estimate),
		}
		if s.Skip {
			continue
		}
		for _, n := range s.Wipes {
			out.Wipes = append(out.Wipes, wipeJSON{Node: n, Step: string(s.ID)})
		}
	}
	return out
}

// syncResultJSON is the fields of the `sync` result object. status is the
// health table read back after the run and is empty when the status step
// never ran.
type syncResultJSON struct {
	Aborted         bool           `json:"aborted"`
	DurationSeconds float64        `json:"duration_seconds"`
	Steps           []syncStepJSON `json:"steps"`
	Status          []statusJSON   `json:"status"`
	Log             string         `json:"log,omitempty"`
}

type syncStepJSON struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Skipped         bool    `json:"skipped"`
	Reason          string  `json:"reason,omitempty"`
	Note            string  `json:"note,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	Error           string  `json:"error,omitempty"`
}

func toSyncResultJSON(r *up.Result, aborted bool, logPath string) syncResultJSON {
	out := syncResultJSON{
		Aborted: aborted,
		Steps:   []syncStepJSON{},
		Status:  []statusJSON{},
	}
	if logPath != "-" {
		out.Log = logPath
	}
	if r == nil {
		return out
	}
	out.DurationSeconds = seconds(r.Duration)
	out.Status = toStatusJSON(r.Status)
	for _, s := range r.Steps {
		out.Steps = append(out.Steps, syncStepJSON{
			ID:              string(s.ID),
			Title:           s.Title,
			Skipped:         s.Skipped,
			Reason:          s.Reason,
			Note:            s.Note,
			DurationSeconds: seconds(s.Duration),
			Error:           errString(s.Err),
		})
	}
	return out
}

func trim(s string) string { return strings.TrimSpace(s) }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// nonNil keeps an empty list as [] rather than null on the wire.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
