package flows

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/llm"
	"github.com/dsbaha/drydock/internal/model"
)

// Classifier triages one item against instructions. *ingest.Filter
// implements this interface; the engine depends only on the signature so the
// ingest package never becomes an import cycle.
type Classifier interface {
	Evaluate(kind, source, title, body, instructions string) (accept bool, reason string)
}

// CreateCardFunc creates a card and autostarts its agent. Implemented by the
// orchestrator's IngestCardWithOptions.
type CreateCardFunc func(boardID, title, body, source, runtime string, skipVerify bool) (*model.Card, error)

// Deps are the engine's collaborators.
type Deps struct {
	DB         *db.DB
	CreateCard CreateCardFunc
	Classify   Classifier // nil → llm_filter nodes fail open (accept)
}

// Item is one inbound unit of work entering a flow.
type Item struct {
	Kind   string // "rss" | "webhook" | "manual"
	Source string // e.g. "webhook:github", "rss:blog", "manual"
	Title  string
	Body   string
}

// Envelope carries an item through the walk. Vars holds llm node outputs;
// CardID is set once a worker node creates the card.
type Envelope struct {
	Item   Item
	CardID string
	Vars   map[string]string
}

// Result reports what a trigger produced.
type Result struct {
	Card     *model.Card // first card created by the walk, if any
	Rejected bool        // an llm_filter node declined the item
	Reason   string      // filter reason (rejections and fail-open accepts)
}

// Engine walks compiled flows.
type Engine struct {
	deps Deps
}

// New builds an Engine.
func New(database *db.DB, createCard CreateCardFunc, classify Classifier) *Engine {
	return &Engine{deps: Deps{DB: database, CreateCard: createCard, Classify: classify}}
}

// SetClassifier swaps the classifier used by llm_filter nodes (wiring aid:
// main builds the engine before the ingest filter exists).
func (e *Engine) SetClassifier(c Classifier) {
	e.deps.Classify = c
}

// flowFor loads the board's flow, seeding the default when none exists.
func (e *Engine) flowFor(boardID string) (*model.Flow, *Definition, error) {
	f, err := e.deps.DB.GetFlow(boardID)
	if err != nil {
		return nil, nil, err
	}
	if f == nil {
		f, err = e.deps.DB.SaveFlow(boardID, "Main flow", DefaultDefinitionJSON())
		if err != nil {
			return nil, nil, err
		}
	}
	def, err := ParseDefinition(f.Definition)
	if err != nil {
		return nil, nil, err
	}
	return f, def, nil
}

// TriggerInbox runs the board's flow for an intake-filtered item.
func (e *Engine) TriggerInbox(boardID, kind, title, body, source string) (*Result, error) {
	return e.Trigger(context.Background(), boardID, InputInbox, Item{Kind: kind, Source: source, Title: title, Body: body})
}

// TriggerManual runs the board's flow for a manually created card.
func (e *Engine) TriggerManual(boardID, title, body string) (*Result, error) {
	return e.Trigger(context.Background(), boardID, InputManual, Item{Kind: "manual", Source: "manual", Title: title, Body: body})
}

// Trigger walks the board's flow from the matching input node(s), carrying
// the item envelope through. It is synchronous; the worker node is
// fire-and-forget (card creation + autostart happen in the orchestrator).
func (e *Engine) Trigger(ctx context.Context, boardID, inputKind string, item Item) (*Result, error) {
	// Opportunistic housekeeping: flow runs older than 30 days expire.
	_ = e.deps.DB.PruneFlowRuns(30 * 86400)

	flow, def, err := e.flowFor(boardID)
	if err != nil {
		return nil, fmt.Errorf("load flow: %w", err)
	}
	g, errs := Compile(def)
	if len(errs) > 0 {
		return nil, fmt.Errorf("flow does not compile: %s", strings.Join(errs, "; "))
	}

	var starts []Node
	for _, n := range def.Nodes {
		if (inputKind == InputInbox && n.Type == NodeInbox) ||
			(inputKind == InputManual && n.Type == NodeManual) {
			starts = append(starts, n)
		}
	}
	if len(starts) == 0 {
		return nil, fmt.Errorf("flow has no %q input node", inputKind)
	}

	res := &Result{}
	w := &walker{engine: e, g: g, boardID: boardID, res: res}
	for _, s := range starts {
		env := Envelope{Item: item, Vars: map[string]string{}}
		if err := w.walk(ctx, env, s.ID, map[string]bool{}); err != nil {
			res.Rejected = false
			e.logRun(flow.ID, boardID, model.FlowRunError, "error: "+err.Error())
			return nil, err
		}
	}

	status, detail := model.FlowRunOK, ""
	switch {
	case res.Rejected:
		status, detail = model.FlowRunRejected, "rejected: "+res.Reason
	case res.Card != nil:
		detail = fmt.Sprintf("card %s created", res.Card.ID)
	default:
		detail = "no card created"
	}
	e.logRun(flow.ID, boardID, status, detail)
	return res, nil
}

func (e *Engine) logRun(flowID, boardID, status, detail string) {
	if err := e.deps.DB.LogFlowRun(flowID, boardID, status, detail); err != nil {
		log.Printf("drydock: log flow run: %v", err)
	}
}

// walker carries walk state.
type walker struct {
	engine  *Engine
	g       *Graph
	boardID string
	res     *Result
}

// walk executes one node and follows its outgoing edges. onPath guards
// against revisiting a node on the same path (the graph is a DAG, but a
// malformed definition that slipped past validation must not hang us).
func (w *walker) walk(ctx context.Context, env Envelope, nodeID string, onPath map[string]bool) error {
	if onPath[nodeID] {
		return fmt.Errorf("cycle detected at node %q", nodeID)
	}
	onPath[nodeID] = true
	defer delete(onPath, nodeID)

	n := w.g.Nodes[nodeID]
	next, err := w.execNode(ctx, &env, n)
	if err != nil {
		return fmt.Errorf("node %q (%s): %w", nodeID, n.Type, err)
	}
	for _, e := range next {
		// Branching copies the envelope so sibling paths don't share vars.
		cpy := env
		cpy.Vars = copyVars(env.Vars)
		if err := w.walk(ctx, cpy, e.To, onPath); err != nil {
			return err
		}
	}
	return nil
}

func copyVars(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// execNode runs one node against the envelope and returns the edges to
// follow next.
func (w *walker) execNode(ctx context.Context, env *Envelope, n Node) ([]Edge, error) {
	switch n.Type {
	case NodeInbox, NodeManual:
		return w.g.OutEdges(n.ID, "out"), nil

	case NodeLLMFilter:
		instructions := cfgString(n.Config, "instructions")
		accept, reason := true, ""
		if w.engine.deps.Classify != nil {
			accept, reason = w.engine.deps.Classify.Evaluate(env.Item.Kind, env.Item.Source, env.Item.Title, env.Item.Body, instructions)
		} else {
			reason = "filter unavailable: no classifier configured"
		}
		if !accept {
			raw := env.Item.Body
			if len([]rune(raw)) > 4000 {
				raw = string([]rune(raw)[:4000]) + "…"
			}
			_ = w.engine.deps.DB.CreateRejectedItem(&model.RejectedItem{
				ID: db.NewID(), Kind: "flow", Source: env.Item.Source,
				Title: env.Item.Title, Reason: reason, Payload: raw,
				BoardID: w.boardID, CreatedAt: db.Now(),
			})
			w.res.Rejected = true
			w.res.Reason = reason
			log.Printf("drydock: flow filter rejected item %q: %s", env.Item.Title, reason)
			return nil, nil
		}
		if strings.HasPrefix(reason, "filter unavailable:") {
			w.res.Reason = reason
		}
		return w.g.OutEdges(n.ID, "accept"), nil

	case NodeLLM:
		prompt := substitute(cfgString(n.Config, "prompt"), env)
		out, err := w.engine.chat(ctx, "You are a text transformer in a Drydock flow. Apply the user's instruction to the item and reply with ONLY the transformed text, no commentary.", prompt)
		if err != nil {
			return nil, err
		}
		out = strings.TrimSpace(out)
		target := cfgString(n.Config, "output")
		if target == "" {
			target = "body"
		}
		if target == "body" {
			env.Item.Body = out
		} else {
			env.Vars[strings.TrimPrefix(target, "var:")] = out
		}
		return w.g.OutEdges(n.ID, "out"), nil

	case NodeWorker:
		runtime := cfgString(n.Config, "runtime")
		verify := cfgBool(n.Config, "verify", true)
		card, err := w.engine.deps.CreateCard(w.boardID, env.Item.Title, env.Item.Body, env.Item.Source, runtime, !verify)
		if err != nil {
			return nil, err
		}
		env.CardID = card.ID
		if w.res.Card == nil {
			w.res.Card = card
		}
		// The out port is optional: unwired, the path ends at the worker.
		return w.g.OutEdges(n.ID, "out"), nil

	case NodeNotify:
		if env.CardID == "" {
			return nil, fmt.Errorf("no card in envelope (notify must come after a worker)")
		}
		if err := w.engine.deps.DB.SetFlowHook(env.CardID, cfgString(n.Config, "url")); err != nil {
			return nil, err
		}
		return nil, nil // terminal
	}
	return nil, fmt.Errorf("unknown node type %q", n.Type)
}

// substitute renders an llm node's prompt template: {{title}}, {{body}} and
// {{var:name}} placeholders.
func substitute(tmpl string, env *Envelope) string {
	out := strings.ReplaceAll(tmpl, "{{title}}", env.Item.Title)
	out = strings.ReplaceAll(out, "{{body}}", env.Item.Body)
	for k, v := range env.Vars {
		out = strings.ReplaceAll(out, "{{var:"+k+"}}", v)
	}
	return out
}

// chat runs one LLM turn against the first admin-configured provider.
func (e *Engine) chat(ctx context.Context, system, user string) (string, error) {
	providers, err := e.deps.DB.ListProviders()
	if err != nil {
		return "", fmt.Errorf("provider lookup failed: %w", err)
	}
	if len(providers) == 0 {
		return "", fmt.Errorf("no LLM provider configured")
	}
	ptype, endpoint, apiKey, model, err := e.deps.DB.GetProviderKey(providers[0].ID)
	if err != nil {
		return "", fmt.Errorf("provider lookup failed: %w", err)
	}
	if apiKey == "" {
		return "", fmt.Errorf("provider has no API key")
	}
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	return llm.Chat(cctx, ptype, endpoint, apiKey, model, system,
		[]llm.Message{{Role: "user", Content: user}}, "")
}

// hookClient posts close notifications; best-effort with a hard timeout.
var hookClient = &http.Client{Timeout: 10 * time.Second}

// FireCloseHooks POSTs {card_id, board_id, title, transcript} to the card's
// registered notify URL (if any), logs the outcome on the card, and deletes
// the hook row. Best-effort: never fails the caller.
func FireCloseHooks(ctx context.Context, database *db.DB, card *model.Card, transcript string) {
	url, err := database.GetFlowHook(card.ID)
	if err != nil || url == "" {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"card_id":    card.ID,
		"board_id":   card.BoardID,
		"title":      card.Title,
		"transcript": transcript,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	outcome := ""
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, derr := hookClient.Do(req)
		if derr != nil {
			outcome = "error: " + derr.Error()
		} else {
			outcome = fmt.Sprintf("HTTP %d", resp.StatusCode)
			resp.Body.Close()
		}
	} else {
		outcome = "error: " + err.Error()
	}
	_ = database.LogEvent(card.ID, "notify_hook", "url="+url+" outcome="+outcome)
	_ = database.DeleteFlowHook(card.ID)
	log.Printf("drydock: notify hook for card %q: %s", card.ID, outcome)
}
