package agents

import (
	"context"
	"sync"
	"time"
)

// EchoRuntime is a demo worker Runtime that needs no LLM key. It walks the
// card's step plan, ticking progress, asks one canned question halfway
// (raising the alarm badge), then completes — so the whole Drydock UI and
// lifecycle is demoable out of the box. Real adapters replace this; see
// docs/agents.md.
type EchoRuntime struct {
	name      string
	stepDelay time.Duration

	mu      sync.Mutex
	paused  bool
	stopped bool
	answerCh chan string
	chatCh   chan string
	stopCh   chan struct{}
}

// NewEchoRuntime builds the demo worker. stepDelay is the pause between
// steps (production default ~1.2s; tests use milliseconds).
func NewEchoRuntime(stepDelay time.Duration) *EchoRuntime {
	if stepDelay <= 0 {
		stepDelay = 1200 * time.Millisecond
	}
	return &EchoRuntime{
		name:     "echo",
		stepDelay: stepDelay,
		answerCh: make(chan string, 1),
		chatCh:   make(chan string, 8),
		stopCh:   make(chan struct{}),
	}
}

func (e *EchoRuntime) Name() string { return e.name }

func (e *EchoRuntime) Run(ctx context.Context, rc *RunContext) (bool, error) {
	emit := func(ev Event) { ev.CardID = rc.Card.ID; rc.Emit.Emit(ev) }

	emit(Event{Kind: EvMessage, From: "agent",
		Text: "Starting work on \"" + rc.Card.Title + "\" — " + itoa(len(rc.Steps)) + " steps planned."})

	asked := rc.QuestionAsked
	half := len(rc.Steps) / 2
	for i, s := range rc.Steps {
		if s.Done {
			continue
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-e.stopCh:
			return false, nil
		case msg := <-e.chatCh:
			emit(Event{Kind: EvMessage, From: "agent", Text: "Noted — \"" + msg + "\". Continuing."})
		case <-time.After(e.stepDelay):
		}
		emit(Event{Kind: EvStepDone, StepID: s.ID})

		// Ask one canned question at the halfway point, unless a question
		// was already asked on this card (e.g. resumed after restart).
		if !asked && i+1 >= half && half > 0 {
			asked = true
			emit(Event{Kind: EvQuestion, From: "worker",
				Text: "Demo checkpoint for \"" + rc.Card.Title + "\": should I optimize for speed or thoroughness? Reply with one word."})
			e.setPaused(true)
			answer, ok := e.waitAnswer(ctx)
			e.setPaused(false)
			if !ok {
				return false, ctx.Err()
			}
			emit(Event{Kind: EvMessage, From: "agent",
				Text: "Got it — \"" + answer + "\". Resuming with that in mind."})
		}
	}

	emit(Event{Kind: EvMessage, From: "agent",
		Text: "All steps complete. Handing off to verification."})
	return true, nil
}

// waitAnswer blocks until the user answers, the run is stopped, or ctx ends.
func (e *EchoRuntime) waitAnswer(ctx context.Context) (string, bool) {
	select {
	case <-ctx.Done():
		return "", false
	case <-e.stopCh:
		return "", false
	case a := <-e.answerCh:
		return a, true
	case msg := <-e.chatCh:
		// A chat message while paused doesn't answer the question; the
		// orchestrator records it and the agent acknowledges via Chat.
		_ = msg
		return e.waitAnswer(ctx)
	}
}

func (e *EchoRuntime) setPaused(p bool) {
	e.mu.Lock()
	e.paused = p
	e.mu.Unlock()
}

// Answer delivers the user's answer to the open question.
func (e *EchoRuntime) Answer(_ context.Context, answer string) error {
	e.mu.Lock()
	paused := e.paused
	e.mu.Unlock()
	if !paused {
		return nil
	}
	select {
	case e.answerCh <- answer:
	default:
	}
	return nil
}

// Chat delivers a free-form user message; the echo agent acknowledges it.
func (e *EchoRuntime) Chat(_ context.Context, text string) error {
	select {
	case e.chatCh <- text:
	default:
	}
	return nil
}

// Stop terminates the run.
func (e *EchoRuntime) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stopped {
		e.stopped = true
		close(e.stopCh)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
