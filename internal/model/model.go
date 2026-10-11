// Package model defines the core domain types shared across packages.
package model

// Card columns (the kanban pipeline).
const (
	ColInbox    = "inbox"
	ColWorking  = "working"
	ColWaiting  = "waiting" // parked: blocked on something external, worker paused
	ColNeedsYou = "needs_you"
	ColQA       = "qa"
	ColDone     = "done"
)

// Columns lists the pipeline in display order.
var Columns = []string{ColInbox, ColWorking, ColWaiting, ColNeedsYou, ColQA, ColDone}

// ValidColumn reports whether c is a known kanban column.
func ValidColumn(c string) bool {
	for _, col := range Columns {
		if c == col {
			return true
		}
	}
	return false
}

// Message senders.
const (
	FromUser   = "user"
	FromAgent  = "agent"
	FromSystem = "system"
)

// Question askers.
const (
	AskedByWorker   = "worker"
	AskedByVerifier = "verifier"
)

// Board is a kanban board. Columns are fixed pipeline stages, not rows.
type Board struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Instructions string `json:"instructions"` // per-board working instructions for agents
	CreatedAt    int64  `json:"created_at"`
}

// Card is a unit of agentic work on the board.
type Card struct {
	ID             string `json:"id"`
	BoardID        string `json:"board_id"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Source         string `json:"source"` // e.g. "webhook:github", "rss:blog", "manual"
	Column         string `json:"column"`
	ProgressPct    int    `json:"progress_pct"`
	ContainerID    string `json:"container_id"`
	AgentRuntime   string `json:"agent_runtime"`
	WaitReason     string `json:"wait_reason"` // why the card sits in waiting ("" when not waiting)
	Transcript     string `json:"transcript"`   // archived markdown transcript, captured at done-time
	SkipVerify     bool   `json:"skip_verify"`  // worker node with verify=false: skip the QA pass
	HasOpenQuestion bool  `json:"has_open_question"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// Step is one unit of a card's plan.
type Step struct {
	ID        string `json:"id"`
	CardID    string `json:"card_id"`
	Label     string `json:"label"`
	Sort      int    `json:"sort"`
	Done      bool   `json:"done"`
	CreatedAt int64  `json:"created_at"`
}

// Question is raised by a worker or verifier agent; while one is open the
// card sits in the needs_you column with the alarm badge.
type Question struct {
	ID         string `json:"id"`
	CardID     string `json:"card_id"`
	Text       string `json:"text"`
	From       string `json:"from"` // worker | verifier
	Answer     string `json:"answer"`
	AnsweredAt int64  `json:"answered_at"`
	CreatedAt  int64  `json:"created_at"`
}

// Open reports whether the question is still awaiting an answer.
func (q *Question) Open() bool { return q.AnsweredAt == 0 }

// Message is one turn of the card-scoped user<->agent chat.
type Message struct {
	ID        string `json:"id"`
	CardID    string `json:"card_id"`
	From      string `json:"from"` // user | agent | system
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
}

// Event is an append-only log entry for a card's lifecycle.
type Event struct {
	ID        string `json:"id"`
	CardID    string `json:"card_id"`
	Kind      string `json:"kind"`
	Payload   string `json:"payload"`
	CreatedAt int64  `json:"created_at"`
}

// Feed is a configured RSS source polled by the ingestor.
type Feed struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	URL               string `json:"url"`
	BoardID           string `json:"board_id"`
	IntervalSec       int    `json:"interval_sec"`
	LastPolledAt      int64  `json:"last_polled_at"`
	LastItemAt        int64  `json:"last_item_at"` // last poll that produced a card
	FilterEnabled     bool   `json:"filter_enabled"`
	FilterInstructions string `json:"filter_instructions"`
	CreatedAt         int64  `json:"created_at"`
}

// WebhookSource is one distinct webhook :source seen in cards, with its card count.
type WebhookSource struct {
	Source             string `json:"source"`
	Count              int    `json:"count"`
	FilterEnabled      bool   `json:"filter_enabled"`
	FilterInstructions string `json:"filter_instructions"`
}

// RejectedItem is an inbound item the intake filter declined. Kept for 30
// days so filter instructions can be tuned; an admin can accept one anyway,
// which creates the card it would have become.
type RejectedItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // "rss" | "webhook"
	Source    string `json:"source"`
	Title     string `json:"title"`
	Reason    string `json:"reason"`
	Payload   string `json:"payload"` // item body (truncated)
	BoardID   string `json:"board_id"`
	CreatedAt int64  `json:"created_at"`
}

// LLMProvider is an admin-configured LLM service agents can use.
// API keys are never exposed to the browser.
type LLMProvider struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ProviderType string `json:"provider_type"` // "openai" or "anthropic"
	Endpoint     string `json:"endpoint"`
	APIKey       string `json:"api_key,omitempty"` // omitted in list responses
	APIKeyMasked string `json:"api_key_masked,omitempty"`
	Model        string `json:"model"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// User is a Drydock account.
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    int64  `json:"created_at"`
}

// WikiPage is one page of a board's persistent wiki: knowledge agents read
// at run start and can propose edits to (via the approval queue).
type WikiPage struct {
	ID        string `json:"id"`
	BoardID   string `json:"board_id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// WikiRevision is one applied version of a wiki page. Appended on every
// applied edit (user edits and approved agent proposals alike) so history
// is never lost.
type WikiRevision struct {
	ID        string `json:"id"`
	PageID    string `json:"page_id"`
	Body      string `json:"body"`
	EditedBy  string `json:"edited_by"` // "user:<username>" or "agent:<card_id>"
	CreatedAt int64  `json:"created_at"`
}

// Wiki proposal statuses.
const (
	ProposalPending  = "pending"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
)

// WikiProposal is an agent-proposed wiki edit awaiting admin approval.
// PageID empty means a proposal for a NEW page titled Title.
type WikiProposal struct {
	ID          string `json:"id"`
	BoardID     string `json:"board_id"`
	PageID      string `json:"page_id"` // "" → new page
	Title       string `json:"title"`
	ProposedBody string `json:"proposed_body"`
	Rationale   string `json:"rationale"`
	CardID      string `json:"card_id"` // proposing run ("" when unknown)
	Status      string `json:"status"`  // pending | approved | rejected
	CreatedAt   int64  `json:"created_at"`
	DecidedAt   int64  `json:"decided_at"`
}

// Flow run statuses.
const (
	FlowRunOK       = "ok"
	FlowRunRejected = "rejected"
	FlowRunError    = "error"
)

// Flow is one board's visual pipeline: how inbound items flow through LLMs
// to outputs. One flow per board (1:1); the definition is JSON (see the
// flows package) and the default flow reproduces the built-in pipeline.
type Flow struct {
	ID         string `json:"id"`
	BoardID    string `json:"board_id"`
	Name       string `json:"name"`
	Definition string `json:"definition"` // JSON: {nodes:[...], edges:[...]}
	UpdatedAt  int64  `json:"updated_at"`
}

// FlowRun is one engine execution, kept for observability (30-day retention).
type FlowRun struct {
	ID        string `json:"id"`
	FlowID    string `json:"flow_id"`
	BoardID   string `json:"board_id"`
	Status    string `json:"status"` // ok | rejected | error
	Detail    string `json:"detail"`
	CreatedAt int64  `json:"created_at"`
}

// FlowHook is a notify-on-close registration: when the card hits done, the
// engine POSTs {card_id, board_id, title, transcript} to URL (best-effort),
// then the row is deleted.
type FlowHook struct {
	CardID string `json:"card_id"`
	URL    string `json:"url"`
}

// AuthSession is a login session (cookie token).
type AuthSession struct {
	Token     string
	UserID    string
	ExpiresAt int64
}
