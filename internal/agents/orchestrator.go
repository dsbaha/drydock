package agents

import (
	"context"
	"fmt"
	"log"
	"strings"
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

// dockerClient is the subset of dockerx.Client the orchestrator uses. Kept
// as an interface so tests can substitute a fake; production passes the real
// *dockerx.Client.
type dockerClient interface {
	StopAndRemove(ctx context.Context, containerID string) error
	RemoveVolume(ctx context.Context, name string) error
	FindCardContainer(ctx context.Context, cardID string) (string, bool, error)
	ImageExists(ctx context.Context, ref string) bool
	PullImage(ctx context.Context, ref string) error
	CreateAndStart(ctx context.Context, opts dockerx.CreateOptions) (string, error)
}

// Orchestrator owns the card lifecycle: it starts worker/verifier runtimes,
// persists their events, provisions per-card containers, and drives column
// transitions.
type Orchestrator struct {
	DB             *db.DB
	Docker         dockerClient // nil when no runtime is reachable
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
	// Normalize a typed-nil *dockerx.Client to a nil interface so
	// o.Docker == nil stays a reliable "no runtime" check.
	var dc dockerClient
	if docker != nil {
		dc = docker
	}
	o := &Orchestrator{
		DB:             database,
		Docker:         dc,
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

// wikiBriefingCap caps the total wiki text included in a run briefing.
const wikiBriefingCap = 12 * 1024

// buildBriefing assembles the run's system context: global working
// instructions, the board's working instructions, and the board's wiki
// pages (capped, with a truncation note). Empty sections are skipped.
func (o *Orchestrator) buildBriefing(boardID string) string {
	var b strings.Builder
	if g, _ := o.DB.GetSetting("instructions"); strings.TrimSpace(g) != "" {
		b.WriteString("# Working instructions (global)\n\n")
		b.WriteString(strings.TrimSpace(g))
		b.WriteString("\n\n")
	}
	if board, _ := o.DB.GetBoard(boardID); board != nil {
		if in := strings.TrimSpace(board.Instructions); in != "" {
			b.WriteString("# Working instructions for board \"" + board.Name + "\"\n\n")
			b.WriteString(in)
			b.WriteString("\n\n")
		}
	}
	pages, _ := o.DB.ListWikiPages(boardID)
	if len(pages) > 0 {
		b.WriteString("# Wiki\n\n")
		used := 0
		for _, p := range pages {
			entry := "## " + p.Title + "\n\n" + strings.TrimSpace(p.Body) + "\n\n"
			if used+len(entry) > wikiBriefingCap {
				b.WriteString("…[wiki truncated: " + itoa(len(pages)) + " pages total, briefing capped]\n")
				break
			}
			b.WriteString(entry)
			used += len(entry)
		}
	}
	return strings.TrimSpace(b.String())
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
			Briefing:      o.buildBriefing(card.BoardID),
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
			o.MoveToDone(cardID)
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

// MoveCard is the manual column move (review accept, triage, waiting, …).
// Moving to waiting parks the card with an optional reason and pauses the
// worker (the run is stopped); the reason is cleared when the card leaves
// waiting. Moving to done removes the card's container (the volume is kept).
// Moving back to working restarts the worker from the remaining steps.
func (o *Orchestrator) MoveCard(cardID, column, reason string) (*model.Card, error) {
	if !model.ValidColumn(column) {
		return nil, fmt.Errorf("unknown column %q", column)
	}
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return nil, fmt.Errorf("card not found")
	}
	o.stopRun(cardID)
	_ = o.DB.SetCardColumn(cardID, column)
	if column == model.ColWaiting {
		_ = o.DB.SetCardWaitReason(cardID, reason)
	} else if card.Column == model.ColWaiting || reason != "" {
		_ = o.DB.SetCardWaitReason(cardID, "")
	}
	_ = o.DB.LogEvent(cardID, "moved", "column="+column)
	switch column {
	case model.ColWorking:
		// Reopening a done card: it starts with a fresh empty volume, so
		// post the archived transcript excerpt for continuity.
		if card.Column == model.ColDone && card.Transcript != "" {
			_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
				From: model.FromSystem,
				Text: "Previous attempt archived transcript:\n\n" +
					truncateRunes(card.Transcript, reopenTranscriptMaxRunes),
				CreatedAt: db.Now()})
		}
		o.StartCard(cardID)
	case model.ColQA:
		o.MoveToQA(cardID)
	case model.ColDone:
		o.MoveToDone(cardID)
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

// StopContainer removes the card's container and its named volume. The chat
// transcript, steps, questions and events already live in the database, so
// nothing of value is lost with the volume.
func (o *Orchestrator) StopContainer(cardID string) error {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return fmt.Errorf("card not found")
	}
	if card.ContainerID == "" || o.Docker == nil {
		return fmt.Errorf("no container to stop")
	}
	detail := o.teardownCard(cardID)
	_ = o.DB.LogEvent(cardID, "container_removed", detail)
	_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
		From: model.FromSystem, Text: "Container and volume removed.", CreatedAt: db.Now()})
	o.refreshNotify(cardID, "updated")
	return nil
}

// teardownCard stops and removes the card's container AND its named volume,
// so no orphaned volumes accumulate. Best-effort: when there is no container,
// no Docker client, or removal fails, it returns a description of what
// happened instead of an error — callers must never fail a transition over
// teardown. Stale container IDs are cleared even when there is nothing to
// remove.
func (o *Orchestrator) teardownCard(cardID string) string {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return "card not found"
	}
	vol := dockerx.VolumeName(cardID)
	if card.ContainerID == "" || o.Docker == nil {
		_ = o.DB.SetCardContainer(cardID, "")
		return "no container"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	removed := card.ContainerID
	detail := "container=" + removed
	if err := o.Docker.StopAndRemove(ctx, removed); err != nil {
		log.Printf("drydock: remove container for card %s: %v", cardID, err)
		detail += " container_error=" + err.Error()
	}
	if err := o.Docker.RemoveVolume(ctx, vol); err != nil {
		log.Printf("drydock: remove volume %s for card %s: %v", vol, cardID, err)
		detail += " volume_error=" + err.Error()
	} else {
		detail += " volume=" + vol + " removed"
	}
	_ = o.DB.SetCardContainer(cardID, "")
	return detail
}

// MoveToDone transitions a card to done: archives a transcript snapshot,
// cancels any live run, marks progress complete, and tears down the card's
// container and volume (see teardownCard). Teardown is best-effort — the done
// transition always succeeds and the outcome is logged, never fatal.
func (o *Orchestrator) MoveToDone(cardID string) {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return
	}
	// Archive the full transcript first: chat, steps, questions and events
	// already live in the database, and this snapshot keeps the done card
	// self-contained even if those tables are pruned later.
	if t := o.buildTranscript(cardID); t != "" {
		_ = o.DB.SetCardTranscript(cardID, t)
	}
	o.stopRun(cardID)
	detail := o.teardownCard(cardID)
	_ = o.DB.LogEvent(cardID, "container_removed", detail)
	_ = o.DB.SetCardColumn(cardID, model.ColDone)
	_ = o.DB.SetCardProgress(cardID, 100)
	_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: cardID,
		From: model.FromSystem, Text: "Container and volume removed.", CreatedAt: db.Now()})
	o.refreshNotify(cardID, "moved")
}

// transcriptMaxRunes bounds the archived transcript stored on a done card.
const transcriptMaxRunes = 64 * 1024

// reopenTranscriptMaxRunes bounds the archived transcript excerpt posted as
// a system message when a done card reopens.
const reopenTranscriptMaxRunes = 8 * 1024

// buildTranscript renders the card's full history — metadata, steps,
// questions with answers, chat messages and the event log — as markdown, so
// a done card stays self-contained in the database after its container and
// volume are gone.
func (o *Orchestrator) buildTranscript(cardID string) string {
	card, err := o.DB.GetCard(cardID)
	if err != nil || card == nil {
		return ""
	}
	var b strings.Builder
	ts := func(t int64) string { return time.Unix(t, 0).UTC().Format(time.RFC3339) }
	fmt.Fprintf(&b, "# %s\n\n", card.Title)
	fmt.Fprintf(&b, "- Source: %s\n- Board: %s\n- Runtime: %s\n- Created: %s\n- Finished: %s\n\n",
		card.Source, card.BoardID, card.AgentRuntime, ts(card.CreatedAt), ts(db.Now()))
	if card.Body != "" {
		fmt.Fprintf(&b, "## Task\n\n%s\n\n", card.Body)
	}
	if steps, err := o.DB.ListSteps(cardID); err == nil && len(steps) > 0 {
		b.WriteString("## Steps\n\n")
		for _, s := range steps {
			mark := "[ ]"
			if s.Done {
				mark = "[x]"
			}
			fmt.Fprintf(&b, "- %s %s\n", mark, s.Label)
		}
		b.WriteString("\n")
	}
	if qs, err := o.DB.ListQuestions(cardID); err == nil && len(qs) > 0 {
		b.WriteString("## Questions\n\n")
		for _, q := range qs {
			fmt.Fprintf(&b, "- **%s** (%s): %s\n", q.From, ts(q.CreatedAt), q.Text)
			if q.Answer != "" {
				fmt.Fprintf(&b, "  - Answer: %s\n", q.Answer)
			}
		}
		b.WriteString("\n")
	}
	if msgs, err := o.DB.ListMessages(cardID, 0); err == nil && len(msgs) > 0 {
		b.WriteString("## Chat\n\n")
		for _, m := range msgs {
			fmt.Fprintf(&b, "**%s** (%s):\n\n%s\n\n", m.From, ts(m.CreatedAt), m.Text)
		}
	}
	if evs, err := o.DB.ListEvents(cardID, 1000); err == nil && len(evs) > 0 {
		b.WriteString("## Events\n\n")
		// ListEvents returns newest-first; render chronologically.
		for i := len(evs) - 1; i >= 0; i-- {
			e := evs[i]
			fmt.Fprintf(&b, "- %s %s %s\n", ts(e.CreatedAt), e.Kind, e.Payload)
		}
		b.WriteString("\n")
	}
	return truncateRunes(b.String(), transcriptMaxRunes)
}

// truncateRunes cuts s to at most n runes, appending an ellipsis marker.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…[truncated]"
}

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

	case EvWait:
		// The worker parked the card: it is blocked on an external
		// response. Stop the run; moving the card back to working
		// restarts the worker from the remaining steps. The runtime
		// should block on ctx.Done() (or return done=false) after
		// emitting this.
		o.stopRun(ev.CardID)
		_ = o.DB.SetCardWaitReason(ev.CardID, ev.Reason)
		_ = o.DB.SetCardColumn(ev.CardID, model.ColWaiting)
		_ = o.DB.LogEvent(ev.CardID, "wait", ev.Reason)
		_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: ev.CardID,
			From: model.FromSystem, Text: "Agent is waiting: " + ev.Reason, CreatedAt: db.Now()})
		o.Notify.NotifyChat(ev.CardID, map[string]any{"kind": "state", "column": model.ColWaiting, "reason": ev.Reason})
		o.refreshNotify(ev.CardID, "moved")

	case EvVerdict:
		if ev.Approved {
			_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: ev.CardID,
				From: model.FromSystem, Text: "Verifier approved: " + ev.Note, CreatedAt: db.Now()})
			_ = o.DB.LogEvent(ev.CardID, "verdict_approved", ev.Note)
			o.MoveToDone(ev.CardID)
		} else {
			_ = o.DB.LogEvent(ev.CardID, "verdict_rejected", ev.Note)
			_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: ev.CardID,
				From: model.FromSystem, Text: "Verifier sent this card back: " + ev.Note, CreatedAt: db.Now()})
			o.StartCard(ev.CardID) // re-enters working with a fresh run
		}

	case EvLog:
		_ = o.DB.LogEvent(ev.CardID, "agent", ev.Text)

	case EvWikiProposal:
		// The agent proposed a wiki edit. It goes to the approval queue;
		// nothing is applied until an admin approves it.
		title := strings.TrimSpace(ev.WikiTitle)
		body := strings.TrimSpace(ev.WikiBody)
		if title == "" || body == "" {
			_ = o.DB.LogEvent(ev.CardID, "wiki_proposal_rejected", "empty title or body")
			break
		}
		card, _ := o.DB.GetCard(ev.CardID)
		if card == nil {
			break
		}
		p := &model.WikiProposal{
			ID: db.NewID(), BoardID: card.BoardID, PageID: ev.WikiPageID,
			Title: title, ProposedBody: body,
			Rationale: strings.TrimSpace(ev.WikiRationale),
			CardID: ev.CardID, Status: model.ProposalPending, CreatedAt: db.Now(),
		}
		if err := o.DB.CreateWikiProposal(p); err != nil {
			_ = o.DB.LogEvent(ev.CardID, "wiki_proposal_rejected", "db: "+err.Error())
			break
		}
		_ = o.DB.LogEvent(ev.CardID, "wiki_proposal", title)
		_ = o.DB.AddMessage(&model.Message{ID: db.NewID(), CardID: ev.CardID,
			From: model.FromSystem, Text: "Proposed wiki edit: \"" + title + "\" — awaiting approval.", CreatedAt: db.Now()})
		o.Notify.NotifyChat(ev.CardID, map[string]any{"kind": "message", "from": model.FromSystem,
			"text": "Proposed wiki edit: \"" + title + "\" — awaiting approval."})
		o.refreshNotify(ev.CardID, "updated")
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
// stopped. Cards waiting on the user (needs_you) keep their open question,
// and cards parked in waiting stay parked; answering / moving them back to
// working restarts the worker.
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
