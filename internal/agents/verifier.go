package agents

import (
	"context"
	"strings"
	"time"
)

// verifierSteps is the demo QA plan.
var verifierSteps = []string{
	"Reproduce the outcome",
	"Check acceptance criteria",
}

// VerifierRuntime is a demo verifier agent: a separate pass over finished
// cards in the qa column. It approves by default; as a demo hook it sends the
// card back to working once when the title contains "flaky". Real verifier
// adapters replace the verdict logic; see docs/agents.md.
type VerifierRuntime struct {
	stepDelay time.Duration
	stopCh    chan struct{}
}

// NewVerifierRuntime builds the demo verifier.
func NewVerifierRuntime(stepDelay time.Duration) *VerifierRuntime {
	if stepDelay <= 0 {
		stepDelay = 900 * time.Millisecond
	}
	return &VerifierRuntime{stepDelay: stepDelay, stopCh: make(chan struct{})}
}

func (v *VerifierRuntime) Name() string { return "verifier" }

func (v *VerifierRuntime) Run(ctx context.Context, rc *RunContext) (bool, error) {
	emit := func(ev Event) { ev.CardID = rc.Card.ID; rc.Emit.Emit(ev) }

	emit(Event{Kind: EvMessage, From: "agent",
		Text: "Verifier here — checking \"" + rc.Card.Title + "\"."})

	for _, label := range verifierSteps {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-v.stopCh:
			return false, nil
		case <-time.After(v.stepDelay):
		}
		emit(Event{Kind: EvLog, Text: "verified step: " + label})
	}

	// Demo hook: a "flaky" card gets one send-back so the full loop
	// (qa → working → qa → done) is exercisable without an LLM.
	if strings.Contains(strings.ToLower(rc.Card.Title), "flaky") && rc.SendBacks == 0 {
		emit(Event{Kind: EvVerdict, Approved: false,
			Note: "Demo verifier: the outcome looked flaky — sending back for another pass."})
		return true, nil
	}

	emit(Event{Kind: EvVerdict, Approved: true,
		Note: "Verified: steps complete and the outcome is consistent."})
	return true, nil
}

func (v *VerifierRuntime) Answer(_ context.Context, _ string) error { return nil }
func (v *VerifierRuntime) Chat(_ context.Context, _ string) error  { return nil }
func (v *VerifierRuntime) Stop()                                    { close(v.stopCh) }
