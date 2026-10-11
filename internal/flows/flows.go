// Package flows implements Drydock's Node-RED-style visual pipelines.
//
// Each board owns one flow: a DAG of nodes that controls how inbound items
// travel through LLMs to outputs. The current hardcoded pipeline (intake
// filter → create card + worker → verifier → done) is the default flow, so
// existing behavior is preserved exactly until a board configures its own.
//
// The engine walks the graph synchronously per trigger; the worker node is
// fire-and-forget (it creates + autostarts the card via the orchestrator and
// returns immediately). There is no durable suspension in v1.
package flows

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Node types (v1 palette).
const (
	NodeInbox     = "inbox"      // input: an item accepted by intake filtering
	NodeManual    = "manual"     // input: the "New card" button
	NodeLLMFilter = "llm_filter" // processor: LLM triage, ports accept/reject
	NodeLLM       = "llm"        // processor: LLM transform, ports in/out
	NodeWorker    = "worker"     // processor: create card + autostart agent (terminal)
	NodeNotify    = "notify"     // output: register close-hook webhook (terminal)
)

// Input kinds select which input node(s) a trigger starts from.
const (
	InputInbox  = "inbox"
	InputManual = "manual"
)

// Node is one box on the canvas.
type Node struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	X      float64        `json:"x"`
	Y      float64        `json:"y"`
	Config map[string]any `json:"config"`
}

// Edge wires an output port to an input port.
type Edge struct {
	From     string `json:"from"`
	FromPort string `json:"from_port"`
	To       string `json:"to"`
	ToPort   string `json:"to_port"`
}

// Definition is the JSON document stored on the flow row.
type Definition struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// ParseDefinition decodes a stored flow definition.
func ParseDefinition(raw string) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("invalid flow definition: %w", err)
	}
	for i := range d.Nodes {
		if d.Nodes[i].Config == nil {
			d.Nodes[i].Config = map[string]any{}
		}
	}
	return &d, nil
}

// portSpec describes a node type's ports.
type portSpec struct {
	inputs  []string
	outputs []string
}

// nodePorts is the v1 palette's port contract.
//
// worker's "out" port is optional: it fires right after the card is created
// (the walk is synchronous; the card's agent keeps working in the
// background). Leaving it unwired ends the path at the worker; wiring it to
// a notify node registers a close-hook for the card. llm_filter's "reject"
// is likewise optional — a declined item is recorded in rejected_items and
// the path simply ends.
var nodePorts = map[string]portSpec{
	NodeInbox:     {inputs: nil, outputs: []string{"out"}},
	NodeManual:    {inputs: nil, outputs: []string{"out"}},
	NodeLLMFilter: {inputs: []string{"in"}, outputs: []string{"accept", "reject"}},
	NodeLLM:       {inputs: []string{"in"}, outputs: []string{"out"}},
	NodeWorker:    {inputs: []string{"in"}, outputs: []string{"out"}},
	NodeNotify:    {inputs: []string{"in"}, outputs: nil},
}

// optionalOut lists output ports that may dangle.
func optionalOut(nodeType, port string) bool {
	return (nodeType == NodeLLMFilter && port == "reject") ||
		(nodeType == NodeWorker && port == "out")
}

// isInputNode reports whether the type starts a flow.
func isInputNode(t string) bool { return t == NodeInbox || t == NodeManual }

// isTerminal reports whether the type ends a path.
func isTerminal(t string) bool { return t == NodeWorker || t == NodeNotify }

// categoryOf colors nodes in the editor: input green, processor blue, output purple.
func categoryOf(t string) string {
	switch t {
	case NodeInbox, NodeManual:
		return "input"
	case NodeNotify:
		return "output"
	default:
		return "processor"
	}
}

// CategoryOf is the exported category lookup for the editor/tests.
func CategoryOf(nodeType string) string { return categoryOf(nodeType) }

// NodeTypes lists the palette in display order.
func NodeTypes() []string {
	return []string{NodeInbox, NodeManual, NodeLLMFilter, NodeLLM, NodeWorker, NodeNotify}
}

// cfgString reads a string config value.
func cfgString(cfg map[string]any, key string) string {
	if cfg == nil {
		return ""
	}
	s, _ := cfg[key].(string)
	return strings.TrimSpace(s)
}

// cfgBool reads a bool config value with a default.
func cfgBool(cfg map[string]any, key string, def bool) bool {
	if cfg == nil {
		return def
	}
	if b, ok := cfg[key].(bool); ok {
		return b
	}
	return def
}

// DefaultDefinition returns the built-in pipeline as a flow: inbox and
// manual inputs each feed a worker (verify on). It reproduces today's
// behavior exactly: create the card in the inbox and auto-start the agent,
// with the verifier pass before done.
func DefaultDefinition() *Definition {
	return &Definition{
		Nodes: []Node{
			{ID: "n-inbox", Type: NodeInbox, X: 60, Y: 80, Config: map[string]any{}},
			{ID: "n-manual", Type: NodeManual, X: 60, Y: 280, Config: map[string]any{}},
			{ID: "n-worker-inbox", Type: NodeWorker, X: 380, Y: 80,
				Config: map[string]any{"runtime": "", "verify": true}},
			{ID: "n-worker-manual", Type: NodeWorker, X: 380, Y: 280,
				Config: map[string]any{"runtime": "", "verify": true}},
		},
		Edges: []Edge{
			{From: "n-inbox", FromPort: "out", To: "n-worker-inbox", ToPort: "in"},
			{From: "n-manual", FromPort: "out", To: "n-worker-manual", ToPort: "in"},
		},
	}
}

// DefaultDefinitionJSON marshals the default definition for storage.
func DefaultDefinitionJSON() string {
	raw, _ := json.Marshal(DefaultDefinition())
	return string(raw)
}

// SummaryLine returns a one-line config summary for the editor's node body.
func SummaryLine(n Node) string {
	switch n.Type {
	case NodeInbox:
		return "accepted intake items"
	case NodeManual:
		return "“New card” button"
	case NodeLLMFilter:
		if s := cfgString(n.Config, "instructions"); s != "" {
			return truncateRunes(s, 48)
		}
		return "no instructions set"
	case NodeLLM:
		out := cfgString(n.Config, "output")
		if out == "" {
			out = "body"
		}
		return "→ " + out
	case NodeWorker:
		rt := cfgString(n.Config, "runtime")
		if rt == "" {
			rt = "default"
		}
		v := "verify on"
		if !cfgBool(n.Config, "verify", true) {
			v = "verify off"
		}
		return rt + " · " + v
	case NodeNotify:
		if u := cfgString(n.Config, "url"); u != "" {
			return u
		}
		return "no URL set"
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
