package agents

import (
	"sync"
	"testing"
	"time"

	"github.com/dsbaha/drydock/internal/db"
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
