package agents

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/dockerx"
	"github.com/dsbaha/drydock/internal/model"
)

// Notifier pushes card and chat updates to the websocket layer. The server
// package implements it.
type Notifier interface {
	// NotifyCard announces a card change; action is one of created, updated,
	// progress, steps, question, answered, message, moved.
	NotifyCard(card *model.Card, action string)
	// NotifyChat pushes a chat-scoped frame ({kind: message|progress|steps|
	// question|state, …}) for one card.
	NotifyChat(cardID string, frame map[string]any)
}

// Orchestrator owns the card lifecycle: it starts worker/verifier runtimes,
// persists their events, provisions per-card containers, and drives column
// transitions.
type Orchestrator struct {
	DB             *db.DB
	Docker         *dockerx.Client // nil when no runtime is reachable
	Image          string
	DefaultRuntime string
	Notify         Notifier

	mu       sync.Mutex
	runs     map[string]*runState
	registry map[string]func() Runtime
}

type runState struct {
	rt     Runtime
	kind   string // "worker" | "verifier"
	cancel context.CancelFunc
}

// New builds an Orchestrator with the demo runtimes registered.
func New(database *db.DB, docker *dockerx.Client, image, defaultRuntime string, notify Notifier) *Orchestrator {
	o := &Orchestrator{
		DB:             database,
		Docker:         docker,
		Image:          image,
		DefaultRuntime: defaultRuntime,
		Notify:         notify,
		runs:           map[string]*runState{},
		registry:       map[string]func() Runtime{},
	}
	o.RegisterRuntime("echo", func() Runtime { return NewEchoRuntime(0) })
	o.RegisterRuntime("verifier", func() Runtime { return NewVerifierRuntime(0) })
	return o
}

// RegisterRuntime adds a named agent adapter ("pi", "zot", …).
func (o *Orchestrator) RegisterRuntime(name string, factory func() Runtime) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.registry[name] = factory
}

func (o *Orchestrator) newRuntime(name string) Runtime {
	o.mu.Lock()
	defer o.mu.Unlock()
	if f, ok := o.registry[name]; ok {
		return f()
	}
	if f, ok := o.registry["echo"]; ok {
		return f()
	}
	return NewEchoRuntime(0)
}

// ---------- ingestion ----------

// IngestCard creates a card in the inbox and auto-starts it.
func (o *Orchestrator) IngestCard(boardID, title, body, source string) (*model.Card, error) {
	now := db.Now()
	card := &model.Card{
		ID:           db.NewID(),
		BoardID:      boardID,
		Title:        title,
		Body:         body,
		Source:       source,
		Column:       model.ColInbox,
		AgentRuntime: o.DefaultRuntime,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := o.DB.CreateCard(card); err != nil {
		return nil, err
	}
	_ = o.DB.LogEvent(card.ID, "ingested", "source="+source)
	o.notifyCard(card, "created")
	o.StartCard(card.ID)
	return o.DB.GetCard(card.ID)
}

// ---------- lifecycle ----------

// StartCard moves a card to working, builds its step plan (reusing history),
// provisions its container, and launches the worker runtime.
func (o *Orchestrator) StartCard(cardID string) {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return
	}
	if card.Column == model.ColDone {
		return
	}
	o.stopRun(cardID)

	// Build the plan from history on a fresh start; resume keeps existing steps.
	steps, err := o.DB.ListSteps(cardID)
	if err == nil && len(steps) == 0 {
		done, _ := o.DB.DoneCardsBySource(card.Source, 10)
		labels := stepTemplate(card, done, o.DB.StepLabels)
		if err := o.DB.CreateSteps(cardID, labels); err == nil {
			steps, _ = o.DB.ListSteps(cardID)
		}
		_ = o.DB.LogEvent(cardID, "planned", fmt.Sprintf("%d steps", len(labels)))
	}

	_ = o.DB.SetCardColumn(cardID, model.ColWorking)
	_ = o.DB.SetCardProgress(cardID, progressOf(steps))
	_ = o.DB.AddMessage(&model.Message{
		ID: db.NewID(), CardID: cardID, From: model.FromSystem,
		Text: "Worker started (" + o.runtimeName(card) + ").", CreatedAt: db.Now(),
	})
	go o.provisionContainer(cardID)
	o.runWorker(cardID)
	o.refreshNotify(cardID, "updated")
}

func (o *Orchestrator) runtimeName(card *model.Card) string {
	if card.AgentRuntime != "" {
		return card.AgentRuntime
	}
	return o.DefaultRuntime
}

func progressOf(steps []*model.Step) int {
	if len(steps) == 0 {
		return 0
	}
	done := 0
	for _, s := range steps {
		if s.Done {
			done++
		}
	}
	return done * 100 / len(steps)
}

func (o *Orchestrator) runWorker(cardID string) {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return
	}
	steps, _ := o.DB.ListSteps(cardID)
	qs, _ := o.DB.ListQuestions(cardID)
	sendBacks, _ := o.DB.CountEvents(cardID, "verdict_rejected")

	ctx, cancel := context.WithCancel(context.Background())
	rt := o.newRuntime(o.runtimeName(card))
	o.setRun(cardID, &runState{rt: rt, kind: "worker", cancel: cancel})

	go func() {
		defer o.clearRun(cardID)
		rc := &RunContext{
			Ctx: ctx, Card: card, Steps: steps, Emit: o,
			ContainerID: card.ContainerID,
			QuestionAsked: len(qs) > 0,
			SendBacks:     sendBacks,
		}
		done, err := rt.Run(ctx, rc)
		switch {
		case err != nil && ctx.Err() == nil:
			_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
				From: model.FromSystem, Text: "Agent error: " + err.Error(), CreatedAt: db.Now()})
			_ = o.DB.SetCardColumn(cardID, model.ColNeedsYou)
			o.refreshNotify(cardID, "updated")
		case done:
			o.MoveToQA(cardID)
		}
	}()
}

// MoveToQA transitions a finished card to the qa column and starts the
// verifier pass.
func (o *Orchestrator) MoveToQA(cardID string) {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil || card.Column == model.ColDone {
		return
	}
	_ = o.DB.SetCardColumn(cardID, model.ColQA)
	_ = o.DB.SetCardProgress(cardID, 100)
	_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
		From: model.FromSystem, Text: "Work finished — verifier is checking it.", CreatedAt: db.Now()})
	o.refreshNotify(cardID, "moved")

	ctx, cancel := context.WithCancel(context.Background())
	rt := o.newRuntime("verifier")
	o.setRun(cardID, &runState{rt: rt, kind: "verifier", cancel: cancel})
	sendBacks, _ := o.DB.CountEvents(cardID, "verdict_rejected")

	go func() {
		defer o.clearRun(cardID)
		rc := &RunContext{Ctx: ctx, Card: card, Emit: o, ContainerID: card.ContainerID, SendBacks: sendBacks}
		done, err := rt.Run(ctx, rc)
		if err != nil && ctx.Err() == nil {
			_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
				From: model.FromSystem, Text: "Verifier error: " + err.Error(), CreatedAt: db.Now()})
		}
		if !done {
			return
		}
		// The verdict event already moved the card; only default to done
		// when the verdict left it in qa (e.g. a custom verifier that just
		// returns done=true).
		if c, _ := o.DB.GetCard(cardID); c != nil && c.Column == model.ColQA {
			_ = o.DB.SetCardColumn(cardID, model.ColDone)
			o.refreshNotify(cardID, "moved")
		}
	}()
}

// SendBack returns a card from QA (or anywhere) to working with a note, and
// restarts the worker.
func (o *Orchestrator) SendBack(cardID, note, from string) error {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return fmt.Errorf("card not found")
	}
	_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
		From: model.FromSystem, Text: from + " sent this card back: " + note, CreatedAt: db.Now()})
	_ = o.DB.LogEvent(cardID, "sent_back", note)
	o.StartCard(cardID)
	return nil
}

// MoveCard is the manual column move (review accept, triage, …).
func (o *Orchestrator) MoveCard(cardID, column string) (*model.Card, error) {
	if !model.ValidColumn(column) {
		return nil, fmt.Errorf("unknown column %q", column)
	}
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return nil, fmt.Errorf("card not found")
	}
	o.stopRun(cardID)
	_ = o.DB.SetCardColumn(cardID, column)
	_ = o.DB.LogEvent(cardID, "moved", "column="+column)
	switch column {
	case model.ColWorking:
		o.StartCard(cardID)
	case model.ColQA:
		o.MoveToQA(cardID)
	}
	return o.DB.GetCard(cardID)
}

// AnswerQuestion records the user's answer and resumes the paused worker. If
// the runtime is gone (e.g. after a restart), the worker is started fresh and
// resumes from the remaining steps.
func (o *Orchestrator) AnswerQuestion(cardID, questionID, answer string) error {
	q, err := o.DB.GetQuestion(questionID)
	if err != nil || q == nil || q.CardID != cardID {
		return fmt.Errorf("question not found")
	}
	if !q.Open() {
		return fmt.Errorf("question already answered")
	}
	if err := o.DB.AnswerQuestion(questionID, answer); err != nil {
		return err
	}
	_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
		From: model.FromUser, Text: "Answer: " + answer, CreatedAt: db.Now()})
	_ = o.DB.LogEvent(cardID, "answered", "question="+questionID)
	_ = o.DB.SetCardOpenQuestion(cardID, false)

	if rt := o.liveRuntime(cardID); rt != nil {
		_ = o.DB.SetCardColumn(cardID, model.ColWorking)
		o.refreshNotify(cardID, "answered")
		return rt.Answer(context.Background(), answer)
	}
	// No live runtime (server restarted while paused): restart the worker;
	// it resumes from the remaining steps and won't re-ask.
	o.StartCard(cardID)
	o.refreshNotify(cardID, "answered")
	return nil
}

// ChatMessage records a user chat message and forwards it to the live agent.
func (o *Orchestrator) ChatMessage(cardID, text string) error {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return fmt.Errorf("card not found")
	}
	_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
		From: model.FromUser, Text: text, CreatedAt: db.Now()})
	o.Notify.NotifyChat(cardID, map[string]any{"kind": "message", "from": model.FromUser, "text": text})
	o.refreshNotify(cardID, "message")
	if rt := o.liveRuntime(cardID); rt != nil {
		return rt.Chat(context.Background(), text)
	}
	_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
		From: model.FromSystem, Text: "No agent is currently running on this card.", CreatedAt: db.Now()})
	return nil
}

// StopContainer stops and removes the card's container. The named volume is
// kept, so /workspace survives.
func (o *Orchestrator) StopContainer(cardID string) error {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return fmt.Errorf("card not found")
	}
	if card.ContainerID == "" || o.Docker == nil {
		return fmt.Errorf("no container to stop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := o.Docker.StopAndRemove(ctx, card.ContainerID); err != nil {
		return err
	}
	_ = o.DB.SetCardContainer(cardID, "")
	_ = o.DB.LogEvent(cardID, "container_stopped", "")
	o.refreshNotify(cardID, "updated")
	return nil
}

// ---------- event handling (Emitter) ----------

// Emit persists a runtime event and fans it out to websockets. It is called
// from runtime goroutines.
func (o *Orchestrator) Emit(ev Event) {
	switch ev.Kind {
	case EvStepDone:
		_ = o.DB.MarkStepDone(ev.StepID)
		steps, _ := o.DB.ListSteps(ev.CardID)
		pct := progressOf(steps)
		_ = o.DB.SetCardProgress(ev.CardID, pct)
		_ = o.DB.LogEvent(ev.CardID, "step_done", ev.StepID)
		o.Notify.NotifyChat(ev.CardID, map[string]any{"kind": "steps"})
		o.Notify.NotifyChat(ev.CardID, map[string]any{"kind": "progress", "pct": pct})
		o.refreshNotify(ev.CardID, "progress")

	case EvMessage:
		_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: ev.CardID,
			From: ev.From, Text: ev.Text, CreatedAt: db.Now()})
		o.Notify.NotifyChat(ev.CardID, map[string]any{"kind": "message", "from": ev.From, "text": ev.Text})
		o.refreshNotify(ev.CardID, "message")

	case EvQuestion:
		q := &model.Question{ID: db.NewID(), CardID: ev.CardID, Text: ev.Text,
			From: ev.From, CreatedAt: db.Now()}
		_ = o.DB.CreateQuestion(q)
		_ = o.DB.SetCardOpenQuestion(ev.CardID, true)
		_ = o.DB.SetCardColumn(ev.CardID, model.ColNeedsYou)
		_ = o.DB.LogEvent(ev.CardID, "question", ev.Text)
		o.Notify.NotifyChat(ev.CardID, map[string]any{"kind": "question", "text": ev.Text})
		o.refreshNotify(ev.CardID, "question")

	case EvVerdict:
		if ev.Approved {
			_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: ev.CardID,
				From: model.FromSystem, Text: "Verifier approved: " + ev.Note, CreatedAt: db.Now()})
			_ = o.DB.LogEvent(ev.CardID, "verdict_approved", ev.Note)
			_ = o.DB.SetCardColumn(ev.CardID, model.ColDone)
			o.refreshNotify(ev.CardID, "moved")
		} else {
			_ = o.DB.LogEvent(ev.CardID, "verdict_rejected", ev.Note)
			_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: ev.CardID,
				From: model.FromSystem, Text: "Verifier sent this card back: " + ev.Note, CreatedAt: db.Now()})
			o.StartCard(ev.CardID) // re-enters working with a fresh run
		}

	case EvLog:
		_ = o.DB.LogEvent(ev.CardID, "agent", ev.Text)
	}
}

// ---------- containers ----------

func (o *Orchestrator) provisionContainer(cardID string) {
	if o.Docker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return
	}
	// Reuse an existing container when one is already there.
	if id, running, err := o.Docker.FindCardContainer(ctx, cardID); err == nil && id != "" && running {
		_ = o.DB.SetCardContainer(cardID, id)
		o.refreshNotify(cardID, "updated")
		return
	}
	if !o.Docker.ImageExists(ctx, o.Image) {
		log.Printf("drydock: pulling image %s for card %s", o.Image, cardID)
		if err := o.Docker.PullImage(ctx, o.Image); err != nil {
			_ = o.DB.LogEvent(cardID, "container_error", "pull: "+err.Error())
			log.Printf("drydock: pull image for card %s: %v", cardID, err)
			return
		}
	}
	id, err := o.Docker.CreateAndStart(ctx, dockerx.CreateOptions{
		CardID: cardID, Image: o.Image, CPU: 1, MemoryMB: 1024,
	})
	if err != nil {
		_ = o.DB.LogEvent(cardID, "container_error", err.Error())
		log.Printf("drydock: create container for card %s: %v", cardID, err)
		return
	}
	_ = o.DB.SetCardContainer(cardID, id)
	o.refreshNotify(cardID, "updated")
}

// ---------- boot ----------

// Reconcile restarts workers for cards that were mid-flight when the server
// stopped. Cards waiting on the user (needs_you) keep their open question;
// answering it restarts the worker.
func (o *Orchestrator) Reconcile(ctx context.Context) {
	_ = ctx
	working, _ := o.DB.ListCardsByColumn(model.ColWorking)
	for _, c := range working {
		o.StartCard(c.ID)
	}
}

// ---------- run bookkeeping ----------

func (o *Orchestrator) setRun(cardID string, rs *runState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.runs[cardID] = rs
}

func (o *Orchestrator) clearRun(cardID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.runs, cardID)
}

func (o *Orchestrator) liveRuntime(cardID string) Runtime {
	o.mu.Lock()
	defer o.mu.Unlock()
	if rs, ok := o.runs[cardID]; ok {
		return rs.rt
	}
	return nil
}

func (o *Orchestrator) stopRun(cardID string) {
	o.mu.Lock()
	rs, ok := o.runs[cardID]
	o.mu.Unlock()
	if ok {
		rs.cancel()
		rs.rt.Stop()
	}
}

func (o *Orchestrator) refreshNotify(cardID, action string) {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return
	}
	o.notifyCard(card, action)
}

func (o *Orchestrator) notifyCard(card *model.Card, action string) {
	if o.Notify != nil {
		o.Notify.NotifyCard(card, action)
	}
}
