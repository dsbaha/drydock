package db

import (
	"database/sql"
	"fmt"

	"github.com/dsbaha/drydock/internal/model"
)

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- users ----------

func (d *DB) CreateUser(u *model.User) error {
	_, err := d.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?)`, u.ID, u.Username, u.PasswordHash, boolInt(u.IsAdmin), u.CreatedAt)
	return err
}

func (d *DB) GetUserByUsername(username string) (*model.User, error) {
	return d.getUser(`WHERE username = ?`, username)
}

func (d *DB) GetUserByID(id string) (*model.User, error) {
	return d.getUser(`WHERE id = ?`, id)
}

func (d *DB) getUser(where string, arg any) (*model.User, error) {
	u := &model.User{}
	var isAdmin int
	err := d.QueryRow(`SELECT id, username, password_hash, is_admin, created_at FROM users `+where, arg).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &isAdmin, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	return u, nil
}

func (d *DB) CountUsers() (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (d *DB) CreateAuthSession(token, userID string, expiresAt int64) error {
	_, err := d.Exec(`INSERT INTO auth_sessions (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID, expiresAt)
	return err
}

func (d *DB) GetAuthSession(token string) (*model.AuthSession, error) {
	s := &model.AuthSession{}
	err := d.QueryRow(`SELECT token, user_id, expires_at FROM auth_sessions WHERE token = ?`, token).
		Scan(&s.Token, &s.UserID, &s.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

func (d *DB) DeleteAuthSession(token string) error {
	_, err := d.Exec(`DELETE FROM auth_sessions WHERE token = ?`, token)
	return err
}

// ---------- boards ----------

func (d *DB) CreateBoard(b *model.Board) error {
	_, err := d.Exec(`INSERT INTO boards (id, name, created_at) VALUES (?, ?, ?)`,
		b.ID, b.Name, b.CreatedAt)
	return err
}

func (d *DB) ListBoards() ([]*model.Board, error) {
	rows, err := d.Query(`SELECT id, name, created_at FROM boards ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Board
	for rows.Next() {
		b := &model.Board{}
		if err := rows.Scan(&b.ID, &b.Name, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (d *DB) GetBoard(id string) (*model.Board, error) {
	b := &model.Board{}
	err := d.QueryRow(`SELECT id, name, created_at FROM boards WHERE id = ?`, id).
		Scan(&b.ID, &b.Name, &b.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

// EnsureDefaultBoard creates the "Main" board when none exists.
func (d *DB) EnsureDefaultBoard() (*model.Board, error) {
	boards, err := d.ListBoards()
	if err != nil {
		return nil, err
	}
	if len(boards) > 0 {
		return boards[0], nil
	}
	b := &model.Board{ID: NewID(), Name: "Main", CreatedAt: Now()}
	if err := d.CreateBoard(b); err != nil {
		return nil, err
	}
	return b, nil
}

// ---------- cards ----------

func scanCard(row interface {
	Scan(...any) error
}) (*model.Card, error) {
	c := &model.Card{}
	var openQ int
	err := row.Scan(&c.ID, &c.BoardID, &c.Title, &c.Body, &c.Source, &c.Column,
		&c.ProgressPct, &c.ContainerID, &c.AgentRuntime, &c.WaitReason, &openQ, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.HasOpenQuestion = openQ == 1
	return c, nil
}

const cardCols = `id, board_id, title, body, source, column_name, progress_pct,
	container_id, agent_runtime, wait_reason, has_open_question, created_at, updated_at`

func (d *DB) CreateCard(c *model.Card) error {
	_, err := d.Exec(`INSERT INTO cards (id, board_id, title, body, source, column_name,
		progress_pct, container_id, agent_runtime, wait_reason, has_open_question, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.BoardID, c.Title, c.Body, c.Source, c.Column, c.ProgressPct,
		c.ContainerID, c.AgentRuntime, c.WaitReason, boolInt(c.HasOpenQuestion), c.CreatedAt, c.UpdatedAt)
	return err
}

func (d *DB) GetCard(id string) (*model.Card, error) {
	c, err := scanCard(d.QueryRow(`SELECT `+cardCols+` FROM cards WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (d *DB) ListCards(boardID string) ([]*model.Card, error) {
	rows, err := d.Query(`SELECT `+cardCols+` FROM cards WHERE board_id = ? ORDER BY updated_at DESC`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCardsByColumn returns cards in a column across all boards (boot
// reconcile, queue draining).
func (d *DB) ListCardsByColumn(column string) ([]*model.Card, error) {
	rows, err := d.Query(`SELECT `+cardCols+` FROM cards WHERE column_name = ? ORDER BY updated_at`, column)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DoneCardsBySource returns completed cards from a source for step-template
// reuse, newest first.
func (d *DB) DoneCardsBySource(source string, limit int) ([]*model.Card, error) {
	rows, err := d.Query(`SELECT `+cardCols+` FROM cards
		WHERE source = ? AND column_name = 'done' ORDER BY updated_at DESC LIMIT ?`, source, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) SetCardColumn(id, column string) error {
	_, err := d.Exec(`UPDATE cards SET column_name = ?, updated_at = ? WHERE id = ?`,
		column, Now(), id)
	return err
}

func (d *DB) SetCardProgress(id string, pct int) error {
	_, err := d.Exec(`UPDATE cards SET progress_pct = ?, updated_at = ? WHERE id = ?`,
		pct, Now(), id)
	return err
}

func (d *DB) SetCardContainer(id, containerID string) error {
	_, err := d.Exec(`UPDATE cards SET container_id = ?, updated_at = ? WHERE id = ?`,
		containerID, Now(), id)
	return err
}

func (d *DB) SetCardOpenQuestion(id string, open bool) error {
	_, err := d.Exec(`UPDATE cards SET has_open_question = ?, updated_at = ? WHERE id = ?`,
		boolInt(open), Now(), id)
	return err
}

// SetCardWaitReason records why a card sits in the waiting column. Callers
// clear it (empty string) when the card leaves waiting.
func (d *DB) SetCardWaitReason(id, reason string) error {
	_, err := d.Exec(`UPDATE cards SET wait_reason = ?, updated_at = ? WHERE id = ?`,
		reason, Now(), id)
	return err
}

func (d *DB) TouchCard(id string) error {
	_, err := d.Exec(`UPDATE cards SET updated_at = ? WHERE id = ?`, Now(), id)
	return err
}

func (d *DB) DeleteCard(id string) error {
	_, err := d.Exec(`DELETE FROM cards WHERE id = ?`, id)
	return err
}

// ---------- steps ----------

func (d *DB) CreateSteps(cardID string, labels []string) error {
	now := Now()
	for i, label := range labels {
		if _, err := d.Exec(`INSERT INTO steps (id, card_id, label, sort, done, created_at)
			VALUES (?, ?, ?, ?, 0, ?)`, NewID(), cardID, label, i, now); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) ReplaceSteps(cardID string, labels []string) error {
	if _, err := d.Exec(`DELETE FROM steps WHERE card_id = ?`, cardID); err != nil {
		return err
	}
	return d.CreateSteps(cardID, labels)
}

func (d *DB) ListSteps(cardID string) ([]*model.Step, error) {
	rows, err := d.Query(`SELECT id, card_id, label, sort, done, created_at FROM steps
		WHERE card_id = ? ORDER BY sort`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Step
	for rows.Next() {
		s := &model.Step{}
		var done int
		if err := rows.Scan(&s.ID, &s.CardID, &s.Label, &s.Sort, &done, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Done = done == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) MarkStepDone(id string) error {
	_, err := d.Exec(`UPDATE steps SET done = 1 WHERE id = ?`, id)
	return err
}

// StepLabels returns just the labels of a card's steps, in order.
func (d *DB) StepLabels(cardID string) ([]string, error) {
	steps, err := d.ListSteps(cardID)
	if err != nil {
		return nil, err
	}
	labels := make([]string, 0, len(steps))
	for _, s := range steps {
		labels = append(labels, s.Label)
	}
	return labels, nil
}

// ---------- questions ----------

func (d *DB) CreateQuestion(q *model.Question) error {
	_, err := d.Exec(`INSERT INTO questions (id, card_id, text, asker, answer, answered_at, created_at)
		VALUES (?, ?, ?, ?, '', 0, ?)`, q.ID, q.CardID, q.Text, q.From, q.CreatedAt)
	return err
}

func (d *DB) GetQuestion(id string) (*model.Question, error) {
	q := &model.Question{}
	err := d.QueryRow(`SELECT id, card_id, text, asker, answer, answered_at, created_at
		FROM questions WHERE id = ?`, id).
		Scan(&q.ID, &q.CardID, &q.Text, &q.From, &q.Answer, &q.AnsweredAt, &q.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return q, err
}

func (d *DB) ListQuestions(cardID string) ([]*model.Question, error) {
	rows, err := d.Query(`SELECT id, card_id, text, asker, answer, answered_at, created_at
		FROM questions WHERE card_id = ? ORDER BY created_at`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Question
	for rows.Next() {
		q := &model.Question{}
		if err := rows.Scan(&q.ID, &q.CardID, &q.Text, &q.From, &q.Answer, &q.AnsweredAt, &q.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// OpenQuestion returns the card's unanswered question, if any.
func (d *DB) OpenQuestion(cardID string) (*model.Question, error) {
	q := &model.Question{}
	err := d.QueryRow(`SELECT id, card_id, text, asker, answer, answered_at, created_at
		FROM questions WHERE card_id = ? AND answered_at = 0 ORDER BY created_at DESC LIMIT 1`, cardID).
		Scan(&q.ID, &q.CardID, &q.Text, &q.From, &q.Answer, &q.AnsweredAt, &q.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return q, err
}

func (d *DB) AnswerQuestion(id, answer string) error {
	_, err := d.Exec(`UPDATE questions SET answer = ?, answered_at = ? WHERE id = ?`,
		answer, Now(), id)
	return err
}

// ---------- messages ----------

func (d *DB) AddMessage(m *model.Message) error {
	_, err := d.Exec(`INSERT INTO messages (id, card_id, sender, text, created_at)
		VALUES (?, ?, ?, ?, ?)`, m.ID, m.CardID, m.From, m.Text, m.CreatedAt)
	return err
}

func (d *DB) ListMessages(cardID string, limit int) ([]*model.Message, error) {
	q := `SELECT id, card_id, sender, text, created_at FROM messages
		WHERE card_id = ? ORDER BY created_at`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := d.Query(q, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Message
	for rows.Next() {
		m := &model.Message{}
		if err := rows.Scan(&m.ID, &m.CardID, &m.From, &m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- events ----------

func (d *DB) LogEvent(cardID, kind, payload string) error {
	_, err := d.Exec(`INSERT INTO events (id, card_id, kind, payload, created_at)
		VALUES (?, ?, ?, ?, ?)`, NewID(), cardID, kind, payload, Now())
	return err
}

func (d *DB) ListEvents(cardID string, limit int) ([]*model.Event, error) {	rows, err := d.Query(`SELECT id, card_id, kind, payload, created_at FROM events
		WHERE card_id = ? ORDER BY created_at DESC LIMIT ?`, cardID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Event
	for rows.Next() {
		e := &model.Event{}
		if err := rows.Scan(&e.ID, &e.CardID, &e.Kind, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- feeds ----------

func (d *DB) CreateFeed(f *model.Feed) error {
	_, err := d.Exec(`INSERT INTO feeds (id, name, url, board_id, interval_sec, last_polled_at, created_at)
		VALUES (?, ?, ?, ?, ?, 0, ?)`, f.ID, f.Name, f.URL, f.BoardID, f.IntervalSec, f.CreatedAt)
	return err
}

func (d *DB) ListFeeds() ([]*model.Feed, error) {
	rows, err := d.Query(`SELECT id, name, url, board_id, interval_sec, last_polled_at, last_item_at, created_at
		FROM feeds ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Feed
	for rows.Next() {
		f := &model.Feed{}
		if err := rows.Scan(&f.ID, &f.Name, &f.URL, &f.BoardID, &f.IntervalSec, &f.LastPolledAt, &f.LastItemAt, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (d *DB) GetFeed(id string) (*model.Feed, error) {
	f := &model.Feed{}
	err := d.QueryRow(`SELECT id, name, url, board_id, interval_sec, last_polled_at, last_item_at, created_at
		FROM feeds WHERE id = ?`, id).
		Scan(&f.ID, &f.Name, &f.URL, &f.BoardID, &f.IntervalSec, &f.LastPolledAt, &f.LastItemAt, &f.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

func (d *DB) DeleteFeed(id string) error {
	_, err := d.Exec(`DELETE FROM feeds WHERE id = ?`, id)
	return err
}

func (d *DB) TouchFeed(id string, at int64) error {
	_, err := d.Exec(`UPDATE feeds SET last_polled_at = ? WHERE id = ?`, at, id)
	return err
}

// SetFeedLastItem records when a feed last produced a card.
func (d *DB) SetFeedLastItem(id string, at int64) error {
	_, err := d.Exec(`UPDATE feeds SET last_item_at = ? WHERE id = ?`, at, id)
	return err
}

// WebhookSources lists the distinct webhook :source names seen in cards,
// with their card counts, for the admin UI.
func (d *DB) WebhookSources() ([]*model.WebhookSource, error) {
	rows, err := d.Query(`SELECT source, COUNT(*) FROM cards
		WHERE source LIKE 'webhook:%' GROUP BY source ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.WebhookSource
	for rows.Next() {
		ws := &model.WebhookSource{}
		if err := rows.Scan(&ws.Source, &ws.Count); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

func (d *DB) FeedSeenGUID(feedID, guid string) (bool, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM feed_items WHERE feed_id = ? AND guid = ?`, feedID, guid).Scan(&n)
	return n > 0, err
}

func (d *DB) FeedMarkSeen(feedID, guid string) error {
	_, err := d.Exec(`INSERT OR IGNORE INTO feed_items (feed_id, guid) VALUES (?, ?)`, feedID, guid)
	return err
}

// ---------- llm providers ----------

func maskKey(key string) string {
	if len(key) <= 8 {
		return "•••"
	}
	return key[:4] + "•••" + key[len(key)-4:]
}

func (d *DB) CreateProvider(p *model.LLMProvider) error {
	_, err := d.Exec(`INSERT INTO llm_providers (id, name, provider_type, endpoint, api_key, model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.ProviderType, p.Endpoint, p.APIKey, p.Model, p.CreatedAt, p.UpdatedAt)
	return err
}

func (d *DB) ListProviders() ([]*model.LLMProvider, error) {
	rows, err := d.Query(`SELECT id, name, provider_type, endpoint, api_key, model, created_at, updated_at
		FROM llm_providers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.LLMProvider
	for rows.Next() {
		p := &model.LLMProvider{}
		var key string
		if err := rows.Scan(&p.ID, &p.Name, &p.ProviderType, &p.Endpoint, &key, &p.Model, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.APIKeyMasked = maskKey(key)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProviderKey returns the raw API key for backend use. Never expose to the browser.
func (d *DB) GetProviderKey(id string) (providerType, endpoint, apiKey, model string, err error) {
	err = d.QueryRow(`SELECT provider_type, endpoint, api_key, model FROM llm_providers WHERE id = ?`, id).
		Scan(&providerType, &endpoint, &apiKey, &model)
	return
}

func (d *DB) DeleteProvider(id string) error {
	_, err := d.Exec(`DELETE FROM llm_providers WHERE id = ?`, id)
	return err
}

// CountEvents counts a card's event-log entries of one kind (used for
// verifier context, e.g. previous send-backs).
func (d *DB) CountEvents(cardID, kind string) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM events WHERE card_id = ? AND kind = ?`, cardID, kind).Scan(&n)
	return n, err
}
