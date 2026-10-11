package agents

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/dockerx"
	"github.com/dsbaha/drydock/internal/model"
)

type stubNotify struct {
	mu      sync.Mutex
	actions []string
}

func (s *stubNotify) NotifyCard(card *model.Card, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, card.Column+":"+action)
}

func (s *stubNotify) NotifyChat(cardID string, frame map[string]any) {}

func testOrchestrator(t *testing.T) (*Orchestrator, *db.DB) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	notify := &stubNotify{}
	o := New(database, nil, "ubuntu:24.04", "echo", notify)
	// Fast demo runtimes for tests.
	o.RegisterRuntime("echo", func() Runtime { return NewEchoRuntime(5 * time.Millisecond) })
	o.RegisterRuntime("verifier", func() Runtime { return NewVerifierRuntime(5 * time.Millisecond) })
	return o, database
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func getCard(t *testing.T, database *db.DB, id string) *model.Card {
	t.Helper()
	card, err := database.GetCard(id)
	if err != nil || card == nil {
		t.Fatalf("get card: %v", err)
	}
	return card
}

// TestFullLifecycle drives a card through the whole pipeline:
// working → question/alarm → answer → qa → done.
func TestFullLifecycle(t *testing.T) {
	o, database := testOrchestrator(t)
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}

	card, err := o.IngestCard(board.ID, "Write the release notes", "Summarize v0.2 changes.", "manual")
	if err != nil {
		t.Fatal(err)
	}

	// Auto-start: card should reach working quickly.
	waitFor(t, "working", 5*time.Second, func() bool {
		return getCard(t, database, card.ID).Column == model.ColWorking
	})

	// Steps are planned from the default template.
	steps, err := database.ListSteps(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != len(DefaultSteps) {
		t.Fatalf("expected %d steps, got %d", len(DefaultSteps), len(steps))
	}

	// The echo worker pauses with a question at ~50%: alarm state.
	waitFor(t, "open question", 5*time.Second, func() bool {
		c := getCard(t, database, card.ID)
		return c.Column == model.ColNeedsYou && c.HasOpenQuestion
	})
	q, err := database.OpenQuestion(card.ID)
	if err != nil || q == nil {
		t.Fatalf("expected open question: %v", err)
	}
	if q.From != model.AskedByWorker {
		t.Fatalf("question from = %q, want worker", q.From)
	}

	// Answering resumes the worker; the card should flow to done via QA.
	if err := o.AnswerQuestion(card.ID, q.ID, "thorough"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "done", 10*time.Second, func() bool {
		return getCard(t, database, card.ID).Column == model.ColDone
	})

	final := getCard(t, database, card.ID)
	if final.ProgressPct != 100 {
		t.Fatalf("progress = %d, want 100", final.ProgressPct)
	}
	if final.HasOpenQuestion {
		t.Fatal("question flag still set after answer")
	}
	steps, _ = database.ListSteps(card.ID)
	for _, s := range steps {
		if !s.Done {
			t.Fatalf("step %q not done", s.Label)
		}
	}
	// The answer is recorded and the chat has agent turns.
	answered, _ := database.GetQuestion(q.ID)
	if answered.Answer != "thorough" {
		t.Fatalf("answer = %q", answered.Answer)
	}
	msgs, _ := database.ListMessages(card.ID, 100)
	agents := 0
	for _, m := range msgs {
		if m.From == model.FromAgent {
			agents++
		}
	}
	if agents < 2 {
		t.Fatalf("expected agent chat turns, got %d messages total", len(msgs))
	}
}

// TestStepHistoryReuse checks that a new card reuses the step plan of a
// similar completed card from the same source.
func TestStepHistoryReuse(t *testing.T) {
	o, database := testOrchestrator(t)
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}

	// Complete card A with a distinctive plan.
	a, err := o.IngestCard(board.ID, "Deploy the api server", "", "webhook:ci")
	if err != nil {
		t.Fatal(err)
	}
	custom := []string{"Build image", "Push to registry", "Roll the deploy"}
	if err := database.ReplaceSteps(a.ID, custom); err != nil {
		t.Fatal(err)
	}
	// Card A pauses with its demo question; answer it so it can finish.
	waitFor(t, "card A question", 5*time.Second, func() bool {
		return getCard(t, database, a.ID).HasOpenQuestion
	})
	qa, _ := database.OpenQuestion(a.ID)
	if err := o.AnswerQuestion(a.ID, qa.ID, "speed"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "card A done", 10*time.Second, func() bool {
		return getCard(t, database, a.ID).Column == model.ColDone
	})

	// Card B has a similar title from the same source: its plan should be
	// cloned from A, not the default template.
	b, err := o.IngestCard(board.ID, "Deploy the api server to staging", "", "webhook:ci")
	if err != nil {
		t.Fatal(err)
	}
	labels, err := database.StepLabels(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != len(custom) {
		t.Fatalf("expected reused plan %v, got %v", custom, labels)
	}
	for i := range custom {
		if labels[i] != custom[i] {
			t.Fatalf("expected reused plan %v, got %v", custom, labels)
		}
	}
}

// ---------- test-only runtimes ----------

// steadyRuntime walks the step plan on a fixed timer and never asks
// questions, so pause/resume timing is deterministic.
type steadyRuntime struct {
	once   sync.Once
	stopCh chan struct{}
}

func newSteadyRuntime() *steadyRuntime { return &steadyRuntime{stopCh: make(chan struct{})} }

func (s *steadyRuntime) Name() string { return "steady" }

func (s *steadyRuntime) Run(ctx context.Context, rc *RunContext) (bool, error) {
	emit := func(ev Event) { ev.CardID = rc.Card.ID; rc.Emit.Emit(ev) }
	for _, st := range rc.Steps {
		if st.Done {
			continue
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-s.stopCh:
			return false, nil
		case <-time.After(50 * time.Millisecond):
		}
		emit(Event{Kind: EvStepDone, StepID: st.ID})
	}
	return true, nil
}

func (s *steadyRuntime) Answer(_ context.Context, _ string) error { return nil }
func (s *steadyRuntime) Chat(_ context.Context, _ string) error   { return nil }
func (s *steadyRuntime) Stop()                                   { s.once.Do(func() { close(s.stopCh) }) }

// waitEmitRuntime parks the card via an EvWait event, then blocks until its
// context is cancelled.
type waitEmitRuntime struct{}

func (w *waitEmitRuntime) Name() string { return "waitemit" }

func (w *waitEmitRuntime) Run(ctx context.Context, rc *RunContext) (bool, error) {
	rc.Emit.Emit(Event{Kind: EvWait, CardID: rc.Card.ID, Reason: "need vendor token"})
	<-ctx.Done()
	return false, nil
}

func (w *waitEmitRuntime) Answer(_ context.Context, _ string) error { return nil }
func (w *waitEmitRuntime) Chat(_ context.Context, _ string) error   { return nil }
func (w *waitEmitRuntime) Stop()                                    {}

// TestWaitingColumn: moving a card to waiting pauses the worker (progress
// freezes) and records the reason; moving back to working clears the reason
// and resumes the worker from the remaining steps.
func TestWaitingColumn(t *testing.T) {
	o, database := testOrchestrator(t)
	o.RegisterRuntime("steady", func() Runtime { return newSteadyRuntime() })
	o.DefaultRuntime = "steady"
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}

	card, err := o.IngestCard(board.ID, "Wait for vendor", "", "manual")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "working", 5*time.Second, func() bool {
		return getCard(t, database, card.ID).Column == model.ColWorking
	})
	// Let the worker make some progress first.
	waitFor(t, "progress", 5*time.Second, func() bool {
		return getCard(t, database, card.ID).ProgressPct > 0
	})

	moved, err := o.MoveCard(card.ID, "waiting", "vendor API is down")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Column != model.ColWaiting {
		t.Fatalf("column = %q, want waiting", moved.Column)
	}
	if moved.WaitReason != "vendor API is down" {
		t.Fatalf("wait_reason = %q, want the reason", moved.WaitReason)
	}

	// The worker is paused: progress must not advance while parked.
	before := getCard(t, database, card.ID).ProgressPct
	time.Sleep(200 * time.Millisecond)
	if after := getCard(t, database, card.ID).ProgressPct; after != before {
		t.Fatalf("progress advanced while waiting: %d -> %d", before, after)
	}

	// Moving back to working clears the reason and resumes to done.
	resumed, err := o.MoveCard(card.ID, "working", "")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Column != model.ColWorking {
		t.Fatalf("column = %q, want working", resumed.Column)
	}
	if resumed.WaitReason != "" {
		t.Fatalf("wait_reason = %q, want cleared", resumed.WaitReason)
	}
	waitFor(t, "done", 10*time.Second, func() bool {
		return getCard(t, database, card.ID).Column == model.ColDone
	})
}

// TestWaitEvent: a runtime-emitted EvWait parks the card in waiting with the
// given reason and stops the run; moving back to working resumes it.
func TestWaitEvent(t *testing.T) {
	o, database := testOrchestrator(t)
	// First run parks the card via EvWait; the resumed run uses the steady
	// worker and completes.
	waited := false
	o.RegisterRuntime("waitonce", func() Runtime {
		if !waited {
			waited = true
			return &waitEmitRuntime{}
		}
		return newSteadyRuntime()
	})
	o.DefaultRuntime = "waitonce"
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}

	card, err := o.IngestCard(board.ID, "Needs a token", "", "manual")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "wait parked", 5*time.Second, func() bool {
		c := getCard(t, database, card.ID)
		return c.Column == model.ColWaiting && c.WaitReason == "need vendor token"
	})

	// Resume: the worker restarts and the card should run to done via QA.
	if _, err := o.MoveCard(card.ID, "working", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "done", 10*time.Second, func() bool {
		return getCard(t, database, card.ID).Column == model.ColDone
	})
	if c := getCard(t, database, card.ID); c.WaitReason != "" {
		t.Fatalf("wait_reason = %q, want cleared", c.WaitReason)
	}
}

// TestVerifierSendback exercises the demo hook: a "flaky" card is sent back
// to working once, then completes.
func TestVerifierSendback(t *testing.T) {
	o, database := testOrchestrator(t)
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	card, err := o.IngestCard(board.ID, "Flaky integration test", "", "manual")
	if err != nil {
		t.Fatal(err)
	}
	// First the worker asks its question; answer it.
	waitFor(t, "open question", 5*time.Second, func() bool {
		return getCard(t, database, card.ID).HasOpenQuestion
	})
	q, _ := database.OpenQuestion(card.ID)
	if err := o.AnswerQuestion(card.ID, q.ID, "speed"); err != nil {
		t.Fatal(err)
	}
	// The verifier sends it back once (column returns to working), then the
	// second pass approves and it reaches done.
	waitFor(t, "done after sendback", 15*time.Second, func() bool {
		return getCard(t, database, card.ID).Column == model.ColDone
	})
	n, _ := database.CountEvents(card.ID, "verdict_rejected")
	if n != 1 {
		t.Fatalf("expected 1 send-back, got %d", n)
	}
}

// TestMoveToDoneRemovesContainer: MoveToDone moves the card to done, clears
// the container ID (best-effort without Docker), logs a container_removed
// event, and posts a system message.
func TestMoveToDoneRemovesContainer(t *testing.T) {
	o, database := testOrchestrator(t)
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	card, err := o.IngestCard(board.ID, "Done cleanup", "", "manual")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a provisioned container (no Docker client in tests).
	if err := database.SetCardContainer(card.ID, "fake-container-id"); err != nil {
		t.Fatal(err)
	}
	o.MoveToDone(card.ID)

	c := getCard(t, database, card.ID)
	if c.Column != model.ColDone {
		t.Fatalf("column = %q, want done", c.Column)
	}
	if c.ContainerID != "" {
		t.Fatalf("ContainerID = %q, want cleared", c.ContainerID)
	}
	if c.ProgressPct != 100 {
		t.Fatalf("progress = %d, want 100", c.ProgressPct)
	}
	events, err := database.ListEvents(card.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, e := range events {
		if e.Kind == "container_removed" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("no container_removed event logged")
	}
	msgs, err := database.ListMessages(card.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen = false
	for _, m := range msgs {
		if m.From == model.FromSystem && m.Text == "Container and volume removed." {
			seen = true
		}
	}
	if !seen {
		t.Fatal("no \"Container and volume removed.\" system message posted")
	}
}

// TestMoveToDoneCapturesTranscript: the done transition archives a markdown
// transcript containing the card's steps, questions (with answers) and chat
// messages, so the card stays self-contained after its volume is gone.
func TestMoveToDoneCapturesTranscript(t *testing.T) {
	o, database := testOrchestrator(t)
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	card, err := o.IngestCard(board.ID, "Transcript archival", "The task body.", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ReplaceSteps(card.ID, []string{"first step", "second step"}); err != nil {
		t.Fatal(err)
	}
	steps, _ := database.ListSteps(card.ID)
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	if err := database.MarkStepDone(steps[0].ID); err != nil {
		t.Fatal(err)
	}
	q := &model.Question{ID: db.NewID(), CardID: card.ID, Text: "Which way?", CreatedAt: db.Now()}
	if err := database.CreateQuestion(q); err != nil {
		t.Fatal(err)
	}
	if err := database.AnswerQuestion(q.ID, "left"); err != nil {
		t.Fatal(err)
	}
	if err := database.AddMessage(&model.Message{ID: db.NewID(), CardID: card.ID,
		From: model.FromUser, Text: "hello agent", CreatedAt: db.Now()}); err != nil {
		t.Fatal(err)
	}

	o.MoveToDone(card.ID)

	c := getCard(t, database, card.ID)
	if c.Transcript == "" {
		t.Fatal("transcript is empty after done")
	}
	for _, want := range []string{
		"# Transcript archival",
		"first step", "second step",
		"Which way?", "left",
		"hello agent",
		"## Steps", "## Questions", "## Chat", "## Events",
	} {
		if !strings.Contains(c.Transcript, want) {
			t.Fatalf("transcript missing %q", want)
		}
	}
	if !strings.Contains(c.Transcript, "[x] first step") || !strings.Contains(c.Transcript, "[ ] second step") {
		t.Fatal("transcript step checklist wrong")
	}
}

// fakeDocker records container/volume teardown calls for tests.
type fakeDocker struct {
	mu      sync.Mutex
	stopped []string
	volumes []string
	managed []dockerx.ManagedVolume
}

func (f *fakeDocker) StopAndRemove(ctx context.Context, containerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, containerID)
	return nil
}

func (f *fakeDocker) RemoveVolume(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes = append(f.volumes, name)
	return nil
}

func (f *fakeDocker) ListManagedVolumes(ctx context.Context) ([]dockerx.ManagedVolume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerx.ManagedVolume, len(f.managed))
	copy(out, f.managed)
	return out, nil
}

func (f *fakeDocker) FindCardContainer(ctx context.Context, cardID string) (string, bool, error) {
	return "", false, nil
}

func (f *fakeDocker) ImageExists(ctx context.Context, ref string) bool { return true }

func (f *fakeDocker) PullImage(ctx context.Context, ref string) error { return nil }

func (f *fakeDocker) CreateAndStart(ctx context.Context, opts dockerx.CreateOptions) (string, error) {
	return "fake-new-container", nil
}

// TestTeardownRemovesContainerAndVolume: with a Docker client present, the
// done transition removes both the container and the card's named volume.
func TestTeardownRemovesContainerAndVolume(t *testing.T) {
	o, database := testOrchestrator(t)
	fake := &fakeDocker{}
	o.Docker = fake
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	card, err := o.IngestCard(board.ID, "Volume teardown", "", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetCardContainer(card.ID, "fake-container-9"); err != nil {
		t.Fatal(err)
	}

	o.MoveToDone(card.ID)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.stopped) != 1 || fake.stopped[0] != "fake-container-9" {
		t.Fatalf("StopAndRemove calls = %v, want [fake-container-9]", fake.stopped)
	}
	wantVol := dockerx.VolumeName(card.ID)
	if len(fake.volumes) != 1 || fake.volumes[0] != wantVol {
		t.Fatalf("RemoveVolume calls = %v, want [%s]", fake.volumes, wantVol)
	}
	if c := getCard(t, database, card.ID); c.ContainerID != "" {
		t.Fatalf("ContainerID = %q, want cleared", c.ContainerID)
	}
}

// TestTeardownBestEffort: a failing Docker client must not fail the done
// transition; the outcome is logged instead.
func TestTeardownBestEffort(t *testing.T) {
	o, database := testOrchestrator(t)
	o.Docker = &failingDocker{}
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	card, err := o.IngestCard(board.ID, "Best effort", "", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetCardContainer(card.ID, "fake-container-x"); err != nil {
		t.Fatal(err)
	}

	o.MoveToDone(card.ID) // must not panic or wedge

	if c := getCard(t, database, card.ID); c.Column != model.ColDone {
		t.Fatalf("column = %q, want done", c.Column)
	}
}

// failingDocker fails every teardown call.
type failingDocker struct{ fakeDocker }

func (f *failingDocker) StopAndRemove(ctx context.Context, containerID string) error {
	return errors.New("fake docker failure")
}

func (f *failingDocker) RemoveVolume(ctx context.Context, name string) error {
	return errors.New("fake docker failure")
}

// TestReopenAfterDone: a card that reaches done through the natural pipeline
// (verifier approval tears down the container and volume) can be moved back
// to working and completes again on a fresh empty volume. The archived
// transcript is posted as a system message so the new run keeps context.
func TestReopenAfterDone(t *testing.T) {
	o, database := testOrchestrator(t)
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	card, err := o.IngestCard(board.ID, "Reopen lifecycle", "", "manual")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a provisioned container so the cleanup path has something to clear.
	if err := database.SetCardContainer(card.ID, "fake-container-1"); err != nil {
		t.Fatal(err)
	}

	// Drive the natural pipeline: working → question → answer → qa → done.
	waitFor(t, "open question", 5*time.Second, func() bool {
		return getCard(t, database, card.ID).HasOpenQuestion
	})
	q, err := database.OpenQuestion(card.ID)
	if err != nil || q == nil {
		t.Fatalf("expected open question: %v", err)
	}
	if err := o.AnswerQuestion(card.ID, q.ID, "thorough"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "done", 10*time.Second, func() bool {
		return getCard(t, database, card.ID).Column == model.ColDone
	})
	if c := getCard(t, database, card.ID); c.ContainerID != "" {
		t.Fatalf("ContainerID = %q after verdict, want cleared", c.ContainerID)
	}

	// Reopen: back to working. The question was already answered, so the
	// worker does not re-ask and the card completes again.
	moved, err := o.MoveCard(card.ID, model.ColWorking, "")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Column != model.ColWorking {
		t.Fatalf("column = %q, want working", moved.Column)
	}
	// The archived transcript was posted as a system message for continuity.
	found := false
	msgs, _ := database.ListMessages(card.ID, 1000)
	for _, m := range msgs {
		if m.From == model.FromSystem && strings.HasPrefix(m.Text, "Previous attempt archived transcript:") {
			found = true
			if !strings.Contains(m.Text, "Reopen lifecycle") {
				t.Fatal("reopen transcript message missing card title")
			}
		}
	}
	if !found {
		t.Fatal("no archived-transcript system message posted on reopen")
	}
	waitFor(t, "done again", 10*time.Second, func() bool {
		return getCard(t, database, card.ID).Column == model.ColDone
	})
	c := getCard(t, database, card.ID)
	if c.ContainerID != "" {
		t.Fatalf("ContainerID = %q after second done, want cleared", c.ContainerID)
	}
	if c.ProgressPct != 100 {
		t.Fatalf("progress = %d, want 100", c.ProgressPct)
	}
	steps, _ := database.ListSteps(card.ID)
	for _, s := range steps {
		if !s.Done {
			t.Fatalf("step %q not done after reopen", s.Label)
		}
	}
}

// orphanTestCard creates a card row directly (no worker run) in the given column.
func orphanTestCard(t *testing.T, database *db.DB, boardID, title, column string) string {
	t.Helper()
	now := db.Now()
	c := &model.Card{
		ID: db.NewID(), BoardID: boardID, Title: title, Body: "", Source: "test",
		Column: column, AgentRuntime: "echo", CreatedAt: now, UpdatedAt: now,
	}
	if err := database.CreateCard(c); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// TestOrphanedVolumes: only volumes for done cards or missing card rows are
// listed; volumes for cards in other columns are never listed.
func TestOrphanedVolumes(t *testing.T) {
	o, database := testOrchestrator(t)
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	doneID := orphanTestCard(t, database, board.ID, "Done task", model.ColDone)
	workingID := orphanTestCard(t, database, board.ID, "Working task", model.ColWorking)
	missingID := db.NewID()
	fake := &fakeDocker{managed: []dockerx.ManagedVolume{
		{Name: "drydock-card-done1", CardID: doneID, CreatedAt: time.Now().Add(-2 * time.Hour)},
		{Name: "drydock-card-work1", CardID: workingID},
		{Name: "drydock-card-miss1", CardID: missingID},
		{Name: "drydock-card-nolabel", CardID: ""},
	}}
	o.Docker = fake
	vols, err := o.OrphanedVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]OrphanedVolume{}
	for _, v := range vols {
		got[v.Name] = v
	}
	for _, want := range []string{"drydock-card-done1", "drydock-card-miss1", "drydock-card-nolabel"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("expected %s to be listed as orphaned", want)
		}
	}
	if _, ok := got["drydock-card-work1"]; ok {
		t.Fatal("working card's volume must never be listed as orphaned")
	}
	if got["drydock-card-done1"].CardTitle != "Done task" {
		t.Fatalf("done volume CardTitle = %q, want %q", got["drydock-card-done1"].CardTitle, "Done task")
	}
	if got["drydock-card-done1"].CreatedAt.IsZero() {
		t.Fatal("done volume CreatedAt not carried through")
	}
}

// TestOrphanedVolumesNoDocker: nil Docker client returns (nil, nil).
func TestOrphanedVolumesNoDocker(t *testing.T) {
	o, _ := testOrchestrator(t)
	vols, err := o.OrphanedVolumes(context.Background())
	if err != nil || vols != nil {
		t.Fatalf("want (nil, nil) without Docker, got (%v, %v)", vols, err)
	}
}

// TestPruneVolumes: deletes exactly the requested orphaned volumes;
// non-orphaned names are skipped, and a volume that stopped being orphaned
// between scan and prune (done card reopened) is skipped too.
func TestPruneVolumes(t *testing.T) {
	o, database := testOrchestrator(t)
	board, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	doneID := orphanTestCard(t, database, board.ID, "Done task", model.ColDone)
	reopenedID := orphanTestCard(t, database, board.ID, "Reopened task", model.ColDone)
	workingID := orphanTestCard(t, database, board.ID, "Working task", model.ColWorking)
	fake := &fakeDocker{managed: []dockerx.ManagedVolume{
		{Name: "drydock-card-a", CardID: doneID},
		{Name: "drydock-card-b", CardID: reopenedID},
		{Name: "drydock-card-c", CardID: workingID},
	}}
	o.Docker = fake
	// Reopen the done card after the scan list is built: prune re-verifies.
	if err := database.SetCardColumn(reopenedID, model.ColWorking); err != nil {
		t.Fatal(err)
	}
	results := o.PruneVolumes(context.Background(), []string{
		"drydock-card-a", "drydock-card-b", "drydock-card-c", "drydock-card-ghost",
	})
	byName := map[string]PruneResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if !byName["drydock-card-a"].OK {
		t.Fatalf("done volume prune failed: %+v", byName["drydock-card-a"])
	}
	for _, n := range []string{"drydock-card-b", "drydock-card-c", "drydock-card-ghost"} {
		if byName[n].OK {
			t.Fatalf("%s must not be pruned (ok=true)", n)
		}
		if byName[n].Error == "" {
			t.Fatalf("%s skipped without an error message", n)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.volumes) != 1 || fake.volumes[0] != "drydock-card-a" {
		t.Fatalf("RemoveVolume called with %v, want only [drydock-card-a]", fake.volumes)
	}
}

// TestPruneVolumesNoDocker: every name fails gracefully without Docker.
func TestPruneVolumesNoDocker(t *testing.T) {
	o, _ := testOrchestrator(t)
	results := o.PruneVolumes(context.Background(), []string{"drydock-card-x"})
	if len(results) != 1 || results[0].OK || results[0].Error == "" {
		t.Fatalf("want graceful failure, got %+v", results)
	}
}
