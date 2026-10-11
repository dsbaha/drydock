package flows

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/model"
)

// ---------- compile ----------

func compileOK(t *testing.T, def *Definition) *Graph {
	t.Helper()
	g, errs := Compile(def)
	if len(errs) > 0 {
		t.Fatalf("expected clean compile, got: %v", errs)
	}
	return g
}

func compileErr(t *testing.T, def *Definition, want string) {
	t.Helper()
	_, errs := Compile(def)
	if len(errs) == 0 {
		t.Fatalf("expected compile errors containing %q, got none", want)
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e, want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected error containing %q, got: %v", want, errs)
	}
}

func node(id, typ string) Node {
	return Node{ID: id, Type: typ, Config: map[string]any{}}
}

func edge(from, fromPort, to, toPort string) Edge {
	return Edge{From: from, FromPort: fromPort, To: to, ToPort: toPort}
}

func TestCompileDefault(t *testing.T) {
	compileOK(t, DefaultDefinition())
}

func TestCompileCycle(t *testing.T) {
	// A 2-cycle needs two wires into one input port, which is rejected by
	// the port rule — so exercise hasCycle directly on a hand-built graph.
	a := node("a", NodeLLM)
	b := node("b", NodeLLM)
	g := &Graph{
		Nodes: map[string]Node{"a": a, "b": b},
		outEdges: map[string][]Edge{
			"a": {edge("a", "out", "b", "in")},
			"b": {edge("b", "out", "a", "in")},
		},
	}
	if !hasCycle(g, nil) {
		t.Fatal("expected cycle detection")
	}
	g2 := &Graph{
		Nodes: map[string]Node{"a": a, "b": b},
		outEdges: map[string][]Edge{
			"a": {edge("a", "out", "b", "in")},
		},
	}
	if hasCycle(g2, nil) {
		t.Fatal("false positive cycle")
	}
	// And end-to-end: a definition whose only flaw is a self-wire is caught.
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), a, node("w", NodeWorker)},
		Edges: []Edge{
			edge("in", "out", "a", "in"),
			edge("a", "out", "a", "in"),
		},
	}
	compileErr(t, def, "wires to itself")
}

func TestCompileUnknownType(t *testing.T) {
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), {ID: "x", Type: "teleport", Config: map[string]any{}}},
		Edges: []Edge{edge("in", "out", "x", "in")},
	}
	compileErr(t, def, "unknown type")
}

func TestCompileUnconnectedIn(t *testing.T) {
	a := node("a", NodeLLM)
	a.Config["prompt"] = "p"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), a, node("w", NodeWorker)},
		Edges: []Edge{edge("a", "out", "w", "in")}, // a.in unwired
	}
	compileErr(t, def, "not connected")
}

func TestCompileDanglingOut(t *testing.T) {
	a := node("a", NodeLLM)
	a.Config["prompt"] = "p"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), a},
		Edges: []Edge{edge("in", "out", "a", "in")}, // a.out dangling: path can't end at llm
	}
	compileErr(t, def, "only worker/notify may terminate")
}

func TestCompileNotifyWithoutWorker(t *testing.T) {
	n := node("n", NodeNotify)
	n.Config["url"] = "https://example.com/hook"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), n},
		Edges: []Edge{edge("in", "out", "n", "in")},
	}
	compileErr(t, def, "without passing through a worker")
}

func TestCompileNotifyAfterWorker(t *testing.T) {
	// The supported shape: worker's optional out port feeds notify.
	n := node("n", NodeNotify)
	n.Config["url"] = "https://example.com/hook"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), node("w", NodeWorker), n},
		Edges: []Edge{
			edge("in", "out", "w", "in"),
			edge("w", "out", "n", "in"),
		},
	}
	compileOK(t, def)
}

func TestCompileNotifySiblingBranch(t *testing.T) {
	// A notify on a sibling branch never passes through a worker → reject.
	f := node("f", NodeLLMFilter)
	f.Config["instructions"] = "accept everything"
	n2 := node("n2", NodeNotify)
	n2.Config["url"] = "https://example.com/hook"
	def2 := &Definition{
		Nodes: []Node{node("in2", NodeInbox), f, node("w2", NodeWorker), n2},
		Edges: []Edge{
			edge("in2", "out", "f", "in"),
			edge("f", "accept", "w2", "in"),
			edge("f", "accept", "n2", "in"),
		},
	}
	compileErr(t, def2, "without passing through a worker")
}

func TestCompileDuplicateInput(t *testing.T) {
	def := &Definition{
		Nodes: []Node{node("a", NodeInbox), node("b", NodeInbox), node("w", NodeWorker)},
		Edges: []Edge{edge("a", "out", "w", "in")},
	}
	compileErr(t, def, "at most one")
}

func TestCompileBadConfig(t *testing.T) {
	f := node("f", NodeLLMFilter) // no instructions
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), f, node("w", NodeWorker)},
		Edges: []Edge{
			edge("in", "out", "f", "in"),
			edge("f", "accept", "w", "in"),
		},
	}
	compileErr(t, def, "instructions are required")

	n := node("n", NodeNotify)
	n.Config["url"] = "not-a-url"
	def2 := &Definition{
		Nodes: []Node{node("in", NodeInbox), node("w", NodeWorker), n},
		Edges: []Edge{edge("in", "out", "w", "in")},
	}
	compileErr(t, def2, "must be an http(s) URL")

	l := node("l", NodeLLM)
	l.Config["prompt"] = "p"
	l.Config["output"] = "carrier-pigeon"
	def3 := &Definition{
		Nodes: []Node{node("in", NodeInbox), l, node("w", NodeWorker)},
		Edges: []Edge{
			edge("in", "out", "l", "in"),
			edge("l", "out", "w", "in"),
		},
	}
	compileErr(t, def3, "output must be")
}

// ---------- engine ----------

type stubClassifier struct {
	accept bool
	reason string
}

func (s stubClassifier) Evaluate(kind, source, title, body, instructions string) (bool, string) {
	return s.accept, s.reason
}

type createdCard struct {
	boardID, title, body, source, runtime string
	skipVerify                            bool
}

type testHarness struct {
	database *db.DB
	created  []*createdCard
	engine   *Engine
}

func newHarness(t *testing.T, classify Classifier) *testHarness {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	h := &testHarness{database: database}
	h.engine = New(database, func(boardID, title, body, source, runtime string, skipVerify bool) (*model.Card, error) {
		h.created = append(h.created, &createdCard{boardID, title, body, source, runtime, skipVerify})
		return &model.Card{ID: db.NewID(), BoardID: boardID, Title: title, Body: body, Source: source}, nil
	}, classify)
	return h
}

func testBoard(t *testing.T, h *testHarness) string {
	t.Helper()
	b, err := h.database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

func TestTriggerDefaultFlow(t *testing.T) {
	h := newHarness(t, stubClassifier{accept: true})
	boardID := testBoard(t, h)
	res, err := h.engine.TriggerInbox(boardID, "webhook", "Hello", "body text", "webhook:github")
	if err != nil {
		t.Fatal(err)
	}
	if res.Card == nil {
		t.Fatal("expected a card")
	}
	if len(h.created) != 1 {
		t.Fatalf("expected 1 card creation, got %d", len(h.created))
	}
	c := h.created[0]
	if c.title != "Hello" || c.body != "body text" || c.source != "webhook:github" || c.skipVerify {
		t.Fatalf("unexpected card: %+v", c)
	}
	runs, err := h.database.ListFlowRuns(boardID, 10)
	if err != nil || len(runs) != 1 || runs[0].Status != model.FlowRunOK {
		t.Fatalf("expected one ok flow run, got %+v, err %v", runs, err)
	}
}

func TestTriggerManual(t *testing.T) {
	h := newHarness(t, stubClassifier{accept: true})
	boardID := testBoard(t, h)
	res, err := h.engine.TriggerManual(boardID, "Manual task", "do it")
	if err != nil {
		t.Fatal(err)
	}
	if res.Card == nil || len(h.created) != 1 || h.created[0].source != "manual" {
		t.Fatalf("unexpected result: %+v created=%+v", res, h.created)
	}
}

func TestTriggerFilterReject(t *testing.T) {
	h := newHarness(t, stubClassifier{accept: false, reason: "spam"})
	boardID := testBoard(t, h)
	// Custom flow: inbox → llm_filter → worker.
	f := node("f", NodeLLMFilter)
	f.Config["instructions"] = "reject spam"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), f, node("w", NodeWorker)},
		Edges: []Edge{
			edge("in", "out", "f", "in"),
			edge("f", "accept", "w", "in"),
		},
	}
	raw, _ := json.Marshal(def)
	if _, err := h.database.SaveFlow(boardID, "t", string(raw)); err != nil {
		t.Fatal(err)
	}
	res, err := h.engine.TriggerInbox(boardID, "rss", "Buy now", "spam body", "rss:spam")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rejected || res.Card != nil {
		t.Fatalf("expected rejection without card, got %+v", res)
	}
	if len(h.created) != 0 {
		t.Fatalf("expected no card creation, got %d", len(h.created))
	}
	items, err := h.database.ListRejectedItems(10, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("expected 1 rejected item, got %+v, err %v", items, err)
	}
	if items[0].Kind != "flow" || items[0].Reason != "spam" {
		t.Fatalf("unexpected rejected item: %+v", items[0])
	}
	runs, _ := h.database.ListFlowRuns(boardID, 10)
	if len(runs) != 1 || runs[0].Status != model.FlowRunRejected {
		t.Fatalf("expected rejected flow run, got %+v", runs)
	}
}

func TestTriggerFilterAccept(t *testing.T) {
	h := newHarness(t, stubClassifier{accept: true, reason: "looks good"})
	boardID := testBoard(t, h)
	f := node("f", NodeLLMFilter)
	f.Config["instructions"] = "accept everything"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), f, node("w", NodeWorker)},
		Edges: []Edge{
			edge("in", "out", "f", "in"),
			edge("f", "accept", "w", "in"),
		},
	}
	raw, _ := json.Marshal(def)
	if _, err := h.database.SaveFlow(boardID, "t", string(raw)); err != nil {
		t.Fatal(err)
	}
	res, err := h.engine.TriggerInbox(boardID, "rss", "Real post", "content", "rss:blog")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected || res.Card == nil || len(h.created) != 1 {
		t.Fatalf("expected accepted card, got %+v created=%d", res, len(h.created))
	}
}

func TestTriggerWorkerVerifyFalse(t *testing.T) {
	h := newHarness(t, stubClassifier{accept: true})
	boardID := testBoard(t, h)
	w := node("w", NodeWorker)
	w.Config["verify"] = false
	w.Config["runtime"] = "echo"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), w},
		Edges: []Edge{edge("in", "out", "w", "in")},
	}
	raw, _ := json.Marshal(def)
	if _, err := h.database.SaveFlow(boardID, "t", string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.TriggerInbox(boardID, "webhook", "T", "B", "webhook:x"); err != nil {
		t.Fatal(err)
	}
	if len(h.created) != 1 || !h.created[0].skipVerify || h.created[0].runtime != "echo" {
		t.Fatalf("unexpected card: %+v", h.created)
	}
}

func chatStub(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":`+jsonQuote(content)+`}}]}`)
	}))
}

func jsonQuote(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

func TestTriggerLLMNode(t *testing.T) {
	h := newHarness(t, stubClassifier{accept: true})
	boardID := testBoard(t, h)
	srv := chatStub(t, "REWRITTEN BODY")
	defer srv.Close()
	p := &model.LLMProvider{ID: db.NewID(), Name: "fake", ProviderType: "openai",
		Endpoint: srv.URL, APIKey: "k", Model: "m", CreatedAt: db.Now(), UpdatedAt: db.Now()}
	if err := h.database.CreateProvider(p); err != nil {
		t.Fatal(err)
	}
	l := node("l", NodeLLM)
	l.Config["prompt"] = "Rewrite: {{title}} / {{body}}"
	l.Config["output"] = "body"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), l, node("w", NodeWorker)},
		Edges: []Edge{
			edge("in", "out", "l", "in"),
			edge("l", "out", "w", "in"),
		},
	}
	raw, _ := json.Marshal(def)
	if _, err := h.database.SaveFlow(boardID, "t", string(raw)); err != nil {
		t.Fatal(err)
	}
	res, err := h.engine.TriggerInbox(boardID, "manual", "Orig", "orig body", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if res.Card == nil || len(h.created) != 1 {
		t.Fatalf("expected card, got %+v", res)
	}
	if h.created[0].body != "REWRITTEN BODY" {
		t.Fatalf("expected rewritten body, got %q", h.created[0].body)
	}
	if h.created[0].title != "Orig" {
		t.Fatalf("title should be untouched, got %q", h.created[0].title)
	}
}

func TestTriggerWorkerNotifyHook(t *testing.T) {
	h := newHarness(t, stubClassifier{accept: true})
	boardID := testBoard(t, h)
	n := node("n", NodeNotify)
	n.Config["url"] = "https://example.com/hook"
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), node("w", NodeWorker), n},
		Edges: []Edge{
			edge("in", "out", "w", "in"),
			edge("w", "out", "n", "in"),
		},
	}
	raw, _ := json.Marshal(def)
	if _, err := h.database.SaveFlow(boardID, "t", string(raw)); err != nil {
		t.Fatal(err)
	}
	res, err := h.engine.TriggerInbox(boardID, "webhook", "T", "B", "webhook:x")
	if err != nil {
		t.Fatal(err)
	}
	if res.Card == nil {
		t.Fatal("expected card")
	}
	u, err := h.database.GetFlowHook(res.Card.ID)
	if err != nil || u != "https://example.com/hook" {
		t.Fatalf("expected hook registered, got %q, err %v", u, err)
	}
}

func TestTriggerNoInput(t *testing.T) {
	h := newHarness(t, stubClassifier{accept: true})
	boardID := testBoard(t, h)
	// Custom flow with only an inbox input; triggering the manual kind fails.
	def := &Definition{
		Nodes: []Node{node("in", NodeInbox), node("w", NodeWorker)},
		Edges: []Edge{edge("in", "out", "w", "in")},
	}
	raw, _ := json.Marshal(def)
	if _, err := h.database.SaveFlow(boardID, "t", string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.TriggerManual(boardID, "T", "B"); err == nil {
		t.Fatal("expected error for missing manual input")
	} else if !strings.Contains(err.Error(), "no \"manual\" input") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFireCloseHooks(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	var got struct {
		path string
		body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	card := &model.Card{ID: db.NewID(), BoardID: "b1", Title: "Hook me"}
	if err := database.SetFlowHook(card.ID, srv.URL+"/hook"); err != nil {
		t.Fatal(err)
	}
	FireCloseHooks(context.Background(), database, card, "## transcript")
	if got.path != "/hook" {
		t.Fatalf("hook not called, got path %q", got.path)
	}
	if got.body["card_id"] != card.ID || got.body["title"] != "Hook me" || got.body["transcript"] != "## transcript" {
		t.Fatalf("unexpected hook payload: %v", got.body)
	}
	// Hook row consumed.
	if u, _ := database.GetFlowHook(card.ID); u != "" {
		t.Fatalf("hook row not deleted, still %q", u)
	}
	// No hook registered → no-op, no panic.
	FireCloseHooks(context.Background(), database, card, "x")
}
