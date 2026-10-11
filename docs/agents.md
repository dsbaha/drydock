# Adding real agent runtimes to Drydock

Drydock ships with two demo runtimes: `echo` (worker) and `verifier` (QA).
Real coding agents — `pi`, `zot`, or anything else — plug in through the
`agents.Runtime` interface. No other code changes are needed.

## The interface

```go
type Runtime interface {
    Name() string
    // Run works the card until it completes (done=true) or the run is
    // stopped (done=false). To ask the user something, emit an EvQuestion
    // event and block inside Run until Answer resumes it.
    Run(ctx context.Context, rc *RunContext) (done bool, err error)
    Answer(ctx context.Context, answer string) error
    Chat(ctx context.Context, text string) error
    Stop()
}
```

`RunContext` carries the card, its step plan (`Steps`), the card's container
ID (`ContainerID`, empty when Docker is unreachable), whether a question was
already asked (`QuestionAsked` — don't ask twice), and an `Emit` sink for
events.

## Events (the only way a runtime talks back)

| Kind         | Fields                          | Effect                                                        |
|--------------|---------------------------------|---------------------------------------------------------------|
| `step_done`  | `StepID`                        | Marks the step done, recomputes progress %, broadcasts        |
| `message`    | `From` (`agent`), `Text`        | Appends to the card chat                                       |
| `question`   | `From` (`worker`/`verifier`), `Text` | Creates the question, moves the card to `needs_you`, raises the 🔔 alarm badge |
| `verdict`    | `Approved`, `Note`              | Verifier only: approve → `done`; reject → back to `working` with the note |
| `log`        | `Text`                          | Appends to the card's event log                               |

Emit them via `rc.Emit.Emit(agents.Event{Kind: agents.EvMessage, ...})` — the
orchestrator fills in `CardID`.

## Writing a pi adapter (sketch)

```go
type PiRuntime struct { proc *exec.Cmd; ... }

func (p *PiRuntime) Run(ctx context.Context, rc *agents.RunContext) (bool, error) {
    // 1. Build the prompt: llm.SystemPrompt(card.Title, card.Body, planText).
    // 2. Pick the provider: db.GetProviderKey(providerID) — keys stay server-side.
    // 3. Launch the agent CLI inside the card's container, e.g.:
    //      docker exec <containerID> pi --prompt "<prompt>" --output-events fd:3
    //    or run it as a sidecar process with /workspace bind-mounted.
    // 4. Stream its stdout: translate tool calls → EvLog, assistant text →
    //    EvMessage, step completions → EvStepDone.
    // 5. When the agent emits a "need input" marker, emit EvQuestion and
    //    block on an internal channel; Answer() delivers the user's reply.
    // 6. When the agent exits 0, return true.
}
```

Register it in `cmd/drydock/main.go` (or a plugin file):

```go
orch.RegisterRuntime("pi", func() agents.Runtime { return NewPiRuntime() })
```

Then set `DRYDOCK_AGENT_RUNTIME=pi` so new cards use it, or set
`agent_runtime` per card.

## Writing a verifier adapter

Same shape, registered as `"verifier"` (it replaces the demo). It should end
its run with exactly one `EvVerdict` event. It may also emit `EvQuestion`
(the card goes to `needs_you` with the alarm badge) when it genuinely needs
the user instead of guessing.

## Conventions

- **Never re-ask.** Check `rc.QuestionAsked` before emitting a question; on
  resume after a server restart the open question is already answered.
- **Steps are the progress bar.** The % badge is `done/total` — emit
  `EvStepDone` as work lands, don't fake it.
- **Containers are optional.** `ContainerID` may be empty (Docker down).
  Degrade gracefully: work in-process or record that the container was
  unavailable.
- **Honor `ctx`.** `Run` must return promptly on cancellation so card moves
  and shutdowns don't hang.
