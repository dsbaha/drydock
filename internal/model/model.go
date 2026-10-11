// Package model defines the core domain types shared across packages.
package model

// Card columns (the kanban pipeline).
const (
	ColInbox    = "inbox"
	ColWorking  = "working"
	ColNeedsYou = "needs_you"
	ColQA       = "qa"
	ColDone     = "done"
)

// Columns lists the pipeline in display order.
var Columns = []string{ColInbox, ColWorking, ColNeedsYou, ColQA, ColDone}

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
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
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
	ID           string `json:"id"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	BoardID      string `json:"board_id"`
	IntervalSec  int    `json:"interval_sec"`
	LastPolledAt int64  `json:"last_polled_at"`
	CreatedAt    int64  `json:"created_at"`
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

// AuthSession is a login session (cookie token).
type AuthSession struct {
	Token     string
	UserID    string
	ExpiresAt int64
}
