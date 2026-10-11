// Package agents runs the agentic task loop behind every Drydock card.
//
// A card's lifecycle: inbox → working (a worker Runtime decomposes the task
// into steps and works them) → needs_you (paused with an open question, alarm
// badge) → qa (a separate verifier Runtime checks the work) → done. The
// verifier can approve, send the card back to working with a note, or raise
// its own question to the user.
//
// Runtimes are deliberately decoupled from storage: they emit Events and the
// Orchestrator persists them (DB writes + websocket broadcast). Real agent
// adapters (pi, zot, …) implement the Runtime interface; see docs/agents.md.
package agents

import (
	"context"

	"github.com/dsbaha/drydock/internal/model"
)

// Event kinds emitted by runtimes (and handled by the Orchestrator).
const (
	EvStepDone = "step_done" // a plan step completed; StepID set
	EvMessage  = "message"   // chat turn; From + Text set
	EvQuestion = "question"  // agent paused with a question; From + Text set
	EvWait     = "wait"      // agent parked the card in waiting; Reason set
	EvVerdict  = "verdict"   // verifier decision; Approved + Note set
	EvLog      = "log"       // event-log entry only; Text set
)

// Event is a single agent action the Orchestrator must persist/broadcast.
type Event struct {
	Kind     string
	CardID   string
	StepID   string
	From     string // message sender / question asker
	Text     string // message or question text
	Reason   string // wait reason (EvWait)
	Approved bool   // verdict
	Note     string // verdict note
}

// Emitter receives runtime events.
type Emitter interface {
	Emit(ev Event)
}

// RunContext is everything a runtime needs for one run.
type RunContext struct {
	Ctx          context.Context
	Card         *model.Card
	Steps        []*model.Step // plan snapshot at run start
	ContainerID  string        // card's container ("" when docker unavailable)
	Emit         Emitter
	QuestionAsked bool // a question was already asked on this card before
	SendBacks    int   // previous verifier send-backs (verifier context)
}

// Runtime is a coding-agent adapter. Implementations must honor ctx
// cancellation in Run and be safe for the Orchestrator to call
// Answer/Chat/Stop from other goroutines.
type Runtime interface {
	// Name identifies the runtime ("echo", "pi", "zot", "verifier", …).
	Name() string
	// Run works the card until it completes (done=true) or the run is
	// stopped (done=false). When the agent needs the user it emits an
	// EvQuestion event and blocks inside Run until Answer resumes it —
	// the card shows the alarm badge meanwhile.
	Run(ctx context.Context, rc *RunContext) (done bool, err error)
	// Answer delivers the user's answer to the open question.
	Answer(ctx context.Context, answer string) error
	// Chat delivers a free-form user message to the agent.
	Chat(ctx context.Context, text string) error
	// Stop terminates the run.
	Stop()
}
