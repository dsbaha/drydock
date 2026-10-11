package flows

import (
	"fmt"
	"net/url"
	"strings"
)

// Graph is a validated, indexed flow definition ready for the engine.
type Graph struct {
	Def   *Definition
	Nodes map[string]Node
	// outEdges[fromID] lists outgoing edges; inEdges[toID] lists incoming.
	outEdges map[string][]Edge
	inEdges  map[string][]Edge
}

// OutEdges returns the edges leaving a node's output port.
func (g *Graph) OutEdges(nodeID, port string) []Edge {
	var out []Edge
	for _, e := range g.outEdges[nodeID] {
		if e.FromPort == port {
			out = append(out, e)
		}
	}
	return out
}

// Compile validates a flow definition and indexes it. It returns the graph
// and, on failure, the list of human-readable problems (empty when valid).
//
// Rules:
//   - every node has a unique id and a known type
//   - at least one input node; at most one per input kind
//   - edges reference existing nodes and valid ports; at most one edge per
//     input port
//   - no cycles
//   - every non-input node's "in" port is connected; llm_filter's "accept"
//     and every other "out" port is connected (llm_filter "reject" may dangle)
//   - every maximal path ends at a worker or notify node
//   - every path to a notify node passes through a worker (a hook needs a card)
//   - per-type config schema (instructions, prompt, output, url, …)
func Compile(def *Definition) (*Graph, []string) {
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	g := &Graph{Def: def, Nodes: map[string]Node{}, outEdges: map[string][]Edge{}, inEdges: map[string][]Edge{}}

	// Node ids unique, types known.
	seenID := map[string]bool{}
	inboxCount, manualCount := 0, 0
	for _, n := range def.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			fail("a node has an empty id")
			continue
		}
		if seenID[n.ID] {
			fail("duplicate node id %q", n.ID)
			continue
		}
		seenID[n.ID] = true
		if _, ok := nodePorts[n.Type]; !ok {
			fail("node %q has unknown type %q", n.ID, n.Type)
			continue
		}
		g.Nodes[n.ID] = n
		switch n.Type {
		case NodeInbox:
			inboxCount++
		case NodeManual:
			manualCount++
		}
	}
	if inboxCount+manualCount == 0 {
		fail("flow needs at least one input node (inbox or manual)")
	}
	if inboxCount > 1 {
		fail("flow has %d inbox inputs; at most one is allowed", inboxCount)
	}
	if manualCount > 1 {
		fail("flow has %d manual inputs; at most one is allowed", manualCount)
	}

	// Edges reference real nodes and ports; one wire per input port.
	wiredIn := map[string]bool{} // "nodeID:port"
	for i, e := range def.Edges {
		from, ok := g.Nodes[e.From]
		if !ok {
			fail("edge %d: unknown from node %q", i, e.From)
			continue
		}
		to, ok := g.Nodes[e.To]
		if !ok {
			fail("edge %d: unknown to node %q", i, e.To)
			continue
		}
		if !hasPort(nodePorts[from.Type].outputs, e.FromPort) {
			fail("edge %d: node %q (%s) has no output port %q", i, e.From, from.Type, e.FromPort)
			continue
		}
		if !hasPort(nodePorts[to.Type].inputs, e.ToPort) {
			fail("edge %d: node %q (%s) has no input port %q", i, e.To, to.Type, e.ToPort)
			continue
		}
		if e.From == e.To {
			fail("edge %d: node %q wires to itself", i, e.From)
			continue
		}
		key := e.To + ":" + e.ToPort
		if wiredIn[key] {
			fail("edge %d: input port %q of node %q already has a wire", i, e.ToPort, e.To)
			continue
		}
		wiredIn[key] = true
		g.outEdges[e.From] = append(g.outEdges[e.From], e)
		g.inEdges[e.To] = append(g.inEdges[e.To], e)
	}

	// Required ports connected.
	for _, n := range def.Nodes {
		spec, ok := nodePorts[n.Type]
		if !ok {
			continue
		}
		for _, p := range spec.inputs {
			if !wiredIn[n.ID+":"+p] {
				fail("node %q (%s): input port %q is not connected", n.ID, n.Type, p)
			}
		}
		for _, p := range spec.outputs {
			// llm_filter's reject port may dangle: the item is still
			// recorded in rejected_items and the path simply ends.
			// worker's out port may dangle: the path ends at the worker.
			if optionalOut(n.Type, p) {
				continue
			}
			if len(g.OutEdges(n.ID, p)) == 0 {
				fail("node %q (%s): output port %q is not connected", n.ID, n.Type, p)
			}
		}
	}

	// No cycles (Kahn's algorithm over node ids).
	if hasCycle(g, def) {
		fail("flow contains a cycle")
	}

	// Every maximal path ends at a worker or notify node: any node with no
	// outgoing edges must be terminal.
	for _, n := range def.Nodes {
		if _, ok := nodePorts[n.Type]; !ok {
			continue
		}
		if len(g.outEdges[n.ID]) == 0 && !isTerminal(n.Type) {
			fail("node %q (%s): path ends here, but only worker/notify may terminate a path", n.ID, n.Type)
		}
	}

	// Every path to a notify node must pass through a worker: walk backwards
	// from each notify without crossing workers; reaching an input means a
	// worker-less path exists (a hook needs a card to attach to).
	for _, n := range def.Nodes {
		if n.Type != NodeNotify {
			continue
		}
		if reachesInputWithoutWorker(g, n.ID) {
			fail("node %q (notify): reachable without passing through a worker — a card must be created first", n.ID)
		}
	}

	// Per-type config schema.
	for _, n := range def.Nodes {
		if _, ok := nodePorts[n.Type]; !ok {
			continue
		}
		switch n.Type {
		case NodeLLMFilter:
			if cfgString(n.Config, "instructions") == "" {
				fail("node %q (llm_filter): instructions are required", n.ID)
			}
		case NodeLLM:
			if cfgString(n.Config, "prompt") == "" {
				fail("node %q (llm): prompt is required", n.ID)
			}
			out := cfgString(n.Config, "output")
			if out == "" {
				out = "body"
			}
			if out != "body" && !strings.HasPrefix(out, "var:") {
				fail("node %q (llm): output must be \"body\" or \"var:<name>\", got %q", n.ID, out)
			} else if strings.HasPrefix(out, "var:") && strings.TrimSpace(strings.TrimPrefix(out, "var:")) == "" {
				fail("node %q (llm): output var name is empty", n.ID)
			}
		case NodeNotify:
			u := cfgString(n.Config, "url")
			if u == "" {
				fail("node %q (notify): url is required", n.ID)
			} else if pu, err := url.Parse(u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
				fail("node %q (notify): url must be an http(s) URL, got %q", n.ID, u)
			}
		}
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return g, nil
}

func hasPort(ports []string, p string) bool {
	for _, q := range ports {
		if q == p {
			return true
		}
	}
	return false
}

// hasCycle reports whether the graph contains a directed cycle.
func hasCycle(g *Graph, def *Definition) bool {
	indeg := map[string]int{}
	for id := range g.Nodes {
		indeg[id] = 0
	}
	for _, edges := range g.outEdges {
		for _, e := range edges {
			indeg[e.To]++
		}
	}
	var queue []string
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for _, e := range g.outEdges[id] {
			indeg[e.To]--
			if indeg[e.To] == 0 {
				queue = append(queue, e.To)
			}
		}
	}
	return seen != len(g.Nodes)
}

// reachesInputWithoutWorker walks backwards from start over reversed edges,
// never crossing a worker node; it reports whether an input node is
// reachable, i.e. a worker-less path to start exists.
func reachesInputWithoutWorker(g *Graph, start string) bool {
	visited := map[string]bool{start: true}
	stack := []string{start}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, e := range g.inEdges[id] {
			prev := g.Nodes[e.From]
			if prev.Type == NodeWorker {
				continue // paths through a worker are fine
			}
			if isInputNode(prev.Type) {
				return true
			}
			if !visited[e.From] {
				visited[e.From] = true
				stack = append(stack, e.From)
			}
		}
	}
	return false
}
