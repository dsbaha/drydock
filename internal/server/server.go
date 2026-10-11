// Package server wires the Drydock HTTP API: REST endpoints for boards,
// cards, ingestion and admin, plus the board event stream and per-card chat
// websockets. It implements agents.Notifier to fan orchestrator events out
// to browsers.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dsbaha/drydock/internal/agents"
	"github.com/dsbaha/drydock/internal/auth"
	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/ingest"
	"github.com/dsbaha/drydock/internal/model"
)

// Server holds the API's dependencies.
type Server struct {
	DB   *db.DB
	Auth *auth.Handler
	Orch *agents.Orchestrator
	Ing  *ingest.Service
	Hub  *Hub
}

// New builds a Server.
func New(database *db.DB, authH *auth.Handler, orch *agents.Orchestrator, ing *ingest.Service) *Server {
	return &Server{DB: database, Auth: authH, Orch: orch, Ing: ing, Hub: NewHub()}
}

// NotifyCard implements agents.Notifier.
func (s *Server) NotifyCard(card *model.Card, action string) {
	s.Hub.Broadcast(card.ID, map[string]any{
		"type": "card", "card_id": card.ID, "action": action, "card": card,
	})
}

// NotifyChat implements agents.Notifier.
func (s *Server) NotifyChat(cardID string, frame map[string]any) {
	frame["card_id"] = cardID
	s.Hub.Broadcast(cardID, frame)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func requireAdmin(s *Server, w http.ResponseWriter, r *http.Request) bool {
	u := auth.UserFromContext(r.Context())
	if u == nil || !u.IsAdmin {
		writeErr(w, http.StatusForbidden, "admin only")
		return false
	}
	return true
}

// cardDetail assembles the flat CardDetail JSON the UI consumes.
func (s *Server) cardDetail(card *model.Card) (map[string]any, error) {
	raw, _ := json.Marshal(card)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	steps, _ := s.DB.ListSteps(card.ID)
	questions, _ := s.DB.ListQuestions(card.ID)
	messages, _ := s.DB.ListMessages(card.ID, 200)
	events, _ := s.DB.ListEvents(card.ID, 50)
	m["steps"] = steps
	m["questions"] = questions
	m["messages"] = messages
	m["events"] = events
	return m, nil
}

func (s *Server) detailFor(w http.ResponseWriter, id string) {
	card, err := s.DB.GetCard(id)
	if err != nil || card == nil {
		writeErr(w, http.StatusNotFound, "card not found")
		return
	}
	d, _ := s.cardDetail(card)
	writeJSON(w, http.StatusOK, d)
}

// Routes builds the authenticated mux.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	authed := s.Auth.RequireAuth

	// Auth (public).
	mux.HandleFunc("POST /api/auth/register", s.Auth.Register)
	mux.HandleFunc("POST /api/auth/login", s.Auth.Login)
	mux.Handle("POST /api/auth/logout", authed(http.HandlerFunc(s.Auth.Logout)))
	mux.Handle("GET /api/auth/me", authed(http.HandlerFunc(s.Auth.Me)))

	// Boards.
	mux.Handle("GET /api/boards", authed(http.HandlerFunc(s.handleBoards)))
	mux.Handle("POST /api/boards", authed(http.HandlerFunc(s.handleCreateBoard)))
	mux.Handle("PUT /api/boards/{id}", authed(http.HandlerFunc(s.handleUpdateBoard)))
	mux.Handle("GET /api/boards/{id}/cards", authed(http.HandlerFunc(s.handleBoardCards)))
	mux.Handle("POST /api/boards/{id}/cards", authed(http.HandlerFunc(s.handleCreateCard)))

	// Board wiki (reads: any user; writes: admin).
	mux.Handle("GET /api/boards/{id}/wiki", authed(http.HandlerFunc(s.handleWikiPages)))
	mux.Handle("POST /api/boards/{id}/wiki", authed(http.HandlerFunc(s.handleCreateWikiPage)))
	mux.Handle("GET /api/boards/{id}/wiki/{pageID}", authed(http.HandlerFunc(s.handleWikiPage)))
	mux.Handle("PUT /api/boards/{id}/wiki/{pageID}", authed(http.HandlerFunc(s.handleUpdateWikiPage)))
	mux.Handle("DELETE /api/boards/{id}/wiki/{pageID}", authed(http.HandlerFunc(s.handleDeleteWikiPage)))

	// Cards.
	mux.Handle("GET /api/cards/{id}", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.detailFor(w, r.PathValue("id"))
	})))
	mux.Handle("POST /api/cards/{id}/messages", authed(http.HandlerFunc(s.handleCardMessage)))
	mux.Handle("POST /api/cards/{id}/answer", authed(http.HandlerFunc(s.handleCardAnswer)))
	mux.Handle("POST /api/cards/{id}/move", authed(http.HandlerFunc(s.handleCardMove)))
	mux.Handle("POST /api/cards/{id}/sendback", authed(http.HandlerFunc(s.handleCardSendback)))
	mux.Handle("POST /api/cards/{id}/stop", authed(http.HandlerFunc(s.handleCardStop)))

	// Ingestion. Webhooks sit behind auth in this build: external systems
	// should call through the same reverse proxy that enforces auth, or use
	// a service account. (Per-source secrets are future work.)
	mux.Handle("POST /api/ingest/webhook/{source}", authed(http.HandlerFunc(s.handleWebhook)))

	// Admin: RSS feeds and LLM providers.
	mux.Handle("GET /api/admin/feeds", authed(http.HandlerFunc(s.handleFeeds)))
	mux.Handle("POST /api/admin/feeds", authed(http.HandlerFunc(s.handleCreateFeed)))
	mux.Handle("PUT /api/admin/feeds/{id}", authed(http.HandlerFunc(s.handleUpdateFeed)))
	mux.Handle("DELETE /api/admin/feeds/{id}", authed(http.HandlerFunc(s.handleDeleteFeed)))
	mux.Handle("GET /api/admin/webhook-sources", authed(http.HandlerFunc(s.handleWebhookSources)))
	mux.Handle("PUT /api/admin/webhook-sources/{source}", authed(http.HandlerFunc(s.handleUpdateWebhookSource)))
	mux.Handle("GET /api/admin/rejected", authed(http.HandlerFunc(s.handleRejected)))
	mux.Handle("POST /api/admin/rejected/{id}/accept", authed(http.HandlerFunc(s.handleAcceptRejected)))
	mux.Handle("GET /api/admin/wiki-proposals", authed(http.HandlerFunc(s.handleWikiProposals)))
	mux.Handle("POST /api/admin/wiki-proposals/{id}/approve", authed(http.HandlerFunc(s.handleApproveWikiProposal)))
	mux.Handle("POST /api/admin/wiki-proposals/{id}/reject", authed(http.HandlerFunc(s.handleRejectWikiProposal)))
	mux.Handle("GET /api/admin/volumes/orphaned", authed(http.HandlerFunc(s.handleOrphanedVolumes)))
	mux.Handle("POST /api/admin/volumes/prune", authed(http.HandlerFunc(s.handlePruneVolumes)))
	mux.Handle("GET /api/admin/settings", authed(http.HandlerFunc(s.handleGetSettings)))
	mux.Handle("PUT /api/admin/settings", authed(http.HandlerFunc(s.handleUpdateSettings)))
	mux.Handle("GET /api/admin/providers", authed(http.HandlerFunc(s.handleProviders)))
	mux.Handle("POST /api/admin/providers", authed(http.HandlerFunc(s.handleCreateProvider)))
	mux.Handle("DELETE /api/admin/providers/{id}", authed(http.HandlerFunc(s.handleDeleteProvider)))

	// Websockets.
	mux.Handle("GET /api/stream", authed(http.HandlerFunc(s.handleStream)))
	mux.Handle("GET /api/cards/{id}/chat", authed(http.HandlerFunc(s.handleCardChat)))

	return mux
}

// ---------- boards ----------

func (s *Server) handleBoards(w http.ResponseWriter, r *http.Request) {
	boards, err := s.DB.ListBoards()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, boards)
}

func (s *Server) handleCreateBoard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Instructions string `json:"instructions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	b := &model.Board{ID: db.NewID(), Name: req.Name,
		Instructions: strings.TrimSpace(req.Instructions), CreatedAt: db.Now()}
	if err := s.DB.CreateBoard(b); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// handleUpdateBoard updates a board's name and/or working instructions (admin only).
func (s *Server) handleUpdateBoard(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	b, err := s.DB.GetBoard(r.PathValue("id"))
	if err != nil || b == nil {
		writeErr(w, http.StatusNotFound, "board not found")
		return
	}
	var req struct {
		Name         *string `json:"name"`
		Instructions *string `json:"instructions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	name := b.Name
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		name = strings.TrimSpace(*req.Name)
	}
	instructions := b.Instructions
	if req.Instructions != nil {
		instructions = strings.TrimSpace(*req.Instructions)
	}
	if err := s.DB.UpdateBoard(b.ID, name, instructions); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	b.Name, b.Instructions = name, instructions
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleBoardCards(w http.ResponseWriter, r *http.Request) {
	board, err := s.DB.GetBoard(r.PathValue("id"))
	if err != nil || board == nil {
		writeErr(w, http.StatusNotFound, "board not found")
		return
	}
	cards, err := s.DB.ListCards(board.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if cards == nil {
		cards = []*model.Card{}
	}
	writeJSON(w, http.StatusOK, cards)
}

func (s *Server) handleCreateCard(w http.ResponseWriter, r *http.Request) {
	board, err := s.DB.GetBoard(r.PathValue("id"))
	if err != nil || board == nil {
		writeErr(w, http.StatusNotFound, "board not found")
		return
	}
	var req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeErr(w, http.StatusBadRequest, "title is required")
		return
	}
	card, err := s.Orch.IngestCard(board.ID, req.Title, req.Body, "manual")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	d, _ := s.cardDetail(card)
	writeJSON(w, http.StatusCreated, d)
}

// ---------- card actions ----------

func (s *Server) handleCardMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	if err := s.Orch.ChatMessage(r.PathValue("id"), req.Text); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCardAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QuestionID string `json:"question_id"`
		Answer     string `json:"answer"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Answer) == "" {
		writeErr(w, http.StatusBadRequest, "answer is required")
		return
	}
	if err := s.Orch.AnswerQuestion(r.PathValue("id"), req.QuestionID, req.Answer); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCardMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Column string `json:"column"`
		Reason string `json:"reason"` // optional: recorded as wait_reason when moving to waiting
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	card, err := s.Orch.MoveCard(r.PathValue("id"), req.Column, req.Reason)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	d, _ := s.cardDetail(card)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleCardSendback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	id := r.PathValue("id")
	if err := s.Orch.SendBack(id, req.Note, "Reviewer"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	card, _ := s.DB.GetCard(id)
	d, _ := s.cardDetail(card)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleCardStop(w http.ResponseWriter, r *http.Request) {
	if err := s.Orch.StopContainer(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- ingestion ----------

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if err := decodeJSON(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	out, err := s.Ing.Webhook(r.PathValue("source"), payload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	if !out.Accepted {
		// Intake filter declined it: recorded for review, nothing to start.
		writeJSON(w, http.StatusAccepted, map[string]any{"accepted": false, "reason": out.Reason})
		return
	}
	d, _ := s.cardDetail(out.Card)
	writeJSON(w, http.StatusCreated, d)
}

// ---------- admin: feeds ----------

func (s *Server) handleFeeds(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	feeds, err := s.DB.ListFeeds()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if feeds == nil {
		feeds = []*model.Feed{}
	}
	writeJSON(w, http.StatusOK, feeds)
}

func (s *Server) handleCreateFeed(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	var req struct {
		Name               string `json:"name"`
		URL                string `json:"url"`
		BoardID            string `json:"board_id"`
		IntervalSec        int    `json:"interval_sec"`
		FilterEnabled      bool   `json:"filter_enabled"`
		FilterInstructions string `json:"filter_instructions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.URL) == "" || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name and url are required")
		return
	}
	boardID := req.BoardID
	if boardID == "" {
		b, err := s.DB.EnsureDefaultBoard()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		boardID = b.ID
	} else if b, _ := s.DB.GetBoard(boardID); b == nil {
		writeErr(w, http.StatusBadRequest, "board not found")
		return
	}
	if req.IntervalSec < 60 {
		req.IntervalSec = 900
	}
	f := &model.Feed{
		ID: db.NewID(), Name: strings.TrimSpace(req.Name), URL: strings.TrimSpace(req.URL),
		BoardID: boardID, IntervalSec: req.IntervalSec,
		FilterEnabled: req.FilterEnabled, FilterInstructions: strings.TrimSpace(req.FilterInstructions),
		CreatedAt: db.Now(),
	}
	if err := s.DB.CreateFeed(f); err != nil {
		writeErr(w, http.StatusBadRequest, "could not create feed (duplicate URL?)")
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// handleUpdateFeed updates a feed's intake-filter toggle and instructions.
func (s *Server) handleUpdateFeed(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	f, err := s.DB.GetFeed(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if f == nil {
		writeErr(w, http.StatusNotFound, "feed not found")
		return
	}
	var req struct {
		FilterEnabled      *bool   `json:"filter_enabled"`
		FilterInstructions *string `json:"filter_instructions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	enabled := f.FilterEnabled
	instructions := f.FilterInstructions
	if req.FilterEnabled != nil {
		enabled = *req.FilterEnabled
	}
	if req.FilterInstructions != nil {
		instructions = strings.TrimSpace(*req.FilterInstructions)
	}
	if err := s.DB.UpdateFeedFilter(f.ID, enabled, instructions); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	f, _ = s.DB.GetFeed(f.ID)
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleDeleteFeed(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	_ = s.DB.DeleteFeed(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleWebhookSources lists the distinct webhook :source names seen in
// cards, with card counts — a quick view of which integrations are feeding
// the board.
func (s *Server) handleWebhookSources(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	sources, err := s.DB.WebhookSources()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if sources == nil {
		sources = []*model.WebhookSource{}
	}
	writeJSON(w, http.StatusOK, sources)
}

// handleUpdateWebhookSource sets a webhook source's intake-filter toggle and
// instructions. The source row is created disabled on first sight, so this
// upserts.
func (s *Server) handleUpdateWebhookSource(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	source := strings.TrimSpace(r.PathValue("source"))
	if source == "" {
		writeErr(w, http.StatusBadRequest, "source is required")
		return
	}
	if !strings.HasPrefix(source, "webhook:") {
		source = "webhook:" + source
	}
	var req struct {
		FilterEnabled      *bool   `json:"filter_enabled"`
		FilterInstructions *string `json:"filter_instructions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	f, err := s.DB.EnsureWebhookFilter(source)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	enabled := f.FilterEnabled
	instructions := f.FilterInstructions
	if req.FilterEnabled != nil {
		enabled = *req.FilterEnabled
	}
	if req.FilterInstructions != nil {
		instructions = strings.TrimSpace(*req.FilterInstructions)
	}
	if err := s.DB.SetWebhookFilter(source, enabled, instructions); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	f, _ = s.DB.EnsureWebhookFilter(source)
	writeJSON(w, http.StatusOK, f)
}

// ---------- admin: rejected items ----------

// handleRejected lists items the intake filter declined, newest first, with
// a count of recent fail-open accepts (filter unavailable) so the admin
// knows to configure an LLM provider.
func (s *Server) handleRejected(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	items, err := s.DB.ListRejectedItems(limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if items == nil {
		items = []*model.RejectedItem{}
	}
	fallbacks, _ := s.DB.CountFilterFallbacks(7 * 86400)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "filter_fallbacks": fallbacks})
}

// handleAcceptRejected creates the card a rejected item would have become
// (auto-started) and removes the rejected row. The tuning loop for writing
// good filter instructions.
func (s *Server) handleAcceptRejected(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	item, err := s.DB.GetRejectedItem(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if item == nil {
		writeErr(w, http.StatusNotFound, "rejected item not found")
		return
	}
	boardID := item.BoardID
	if boardID == "" {
		b, err := s.DB.EnsureDefaultBoard()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		boardID = b.ID
	}
	card, err := s.Orch.IngestCard(boardID, item.Title, item.Payload, item.Source)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create card")
		return
	}
	_ = s.DB.DeleteRejectedItem(item.ID)
	d, _ := s.cardDetail(card)
	writeJSON(w, http.StatusCreated, d)
}

// ---------- admin: llm providers ----------

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	providers, err := s.DB.ListProviders()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if providers == nil {
		providers = []*model.LLMProvider{}
	}
	writeJSON(w, http.StatusOK, providers)
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	var req struct {
		Name         string `json:"name"`
		ProviderType string `json:"provider_type"`
		Endpoint     string `json:"endpoint"`
		APIKey       string `json:"api_key"`
		Model        string `json:"model"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.ProviderType != "anthropic" {
		req.ProviderType = "openai" // also covers OpenAI-compatible endpoints
	}
	now := db.Now()
	p := &model.LLMProvider{
		ID: db.NewID(), Name: strings.TrimSpace(req.Name), ProviderType: req.ProviderType,
		Endpoint: strings.TrimSpace(req.Endpoint), APIKey: req.APIKey,
		Model: strings.TrimSpace(req.Model), CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.CreateProvider(p); err != nil {
		writeErr(w, http.StatusBadRequest, "could not create provider (duplicate name?)")
		return
	}
	p.APIKey = ""
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	_ = s.DB.DeleteProvider(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- websockets ----------

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleStream is the board event stream: every card and chat frame.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.WriteJSON(map[string]string{"type": "hello"})

	ch, unsub := s.Hub.Subscribe("")
	defer unsub()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case f := <-ch:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, f.Data); err != nil {
				return
			}
		}
	}
}

// handleCardChat is the per-card chat socket: history on open, live frames
// after, {"text": "..."} inbound from the browser.
func (s *Server) handleCardChat(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("id")
	card, err := s.DB.GetCard(cardID)
	if err != nil || card == nil {
		writeErr(w, http.StatusNotFound, "card not found")
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	messages, _ := s.DB.ListMessages(cardID, 200)
	if messages == nil {
		messages = []*model.Message{}
	}
	_ = conn.WriteJSON(map[string]any{"kind": "history", "card_id": cardID, "messages": messages})

	ch, unsub := s.Hub.Subscribe(cardID)
	defer unsub()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var inbound struct {
				Text string `json:"text"`
			}
			if err := conn.ReadJSON(&inbound); err != nil {
				return
			}
			if strings.TrimSpace(inbound.Text) == "" {
				continue
			}
			_ = s.Orch.ChatMessage(cardID, inbound.Text)
		}
	}()
	for {
		select {
		case <-done:
			return
		case f := <-ch:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, f.Data); err != nil {
				return
			}
		}
	}
}

// ---------- board wiki ----------

// boardFromWiki resolves the board for wiki routes and 404s when missing.
func (s *Server) boardFromWiki(w http.ResponseWriter, r *http.Request) *model.Board {
	b, err := s.DB.GetBoard(r.PathValue("id"))
	if err != nil || b == nil {
		writeErr(w, http.StatusNotFound, "board not found")
		return nil
	}
	return b
}

// handleWikiPages lists a board's wiki pages (any authenticated user).
func (s *Server) handleWikiPages(w http.ResponseWriter, r *http.Request) {
	b := s.boardFromWiki(w, r)
	if b == nil {
		return
	}
	pages, err := s.DB.ListWikiPages(b.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if pages == nil {
		pages = []*model.WikiPage{}
	}
	// List view doesn't need full bodies.
	type summary struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		UpdatedAt int64  `json:"updated_at"`
	}
	out := make([]summary, 0, len(pages))
	for _, p := range pages {
		out = append(out, summary{ID: p.ID, Title: p.Title, UpdatedAt: p.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWikiPage returns one page plus its recent revisions.
func (s *Server) handleWikiPage(w http.ResponseWriter, r *http.Request) {
	b := s.boardFromWiki(w, r)
	if b == nil {
		return
	}
	p, err := s.DB.GetWikiPage(r.PathValue("pageID"))
	if err != nil || p == nil || p.BoardID != b.ID {
		writeErr(w, http.StatusNotFound, "page not found")
		return
	}
	revs, _ := s.DB.ListWikiRevisions(p.ID, 20)
	if revs == nil {
		revs = []*model.WikiRevision{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"page": p, "revisions": revs})
}

// handleCreateWikiPage creates a page (admin only) and records a revision.
func (s *Server) handleCreateWikiPage(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	b := s.boardFromWiki(w, r)
	if b == nil {
		return
	}
	var req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeErr(w, http.StatusBadRequest, "title is required")
		return
	}
	u := auth.UserFromContext(r.Context())
	p := &model.WikiPage{ID: db.NewID(), BoardID: b.ID, Title: req.Title,
		Body: req.Body, CreatedAt: db.Now(), UpdatedAt: db.Now()}
	if err := s.DB.CreateWikiPage(p, "user:"+u.Username); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// handleUpdateWikiPage updates title/body (admin only); every applied edit
// appends a revision.
func (s *Server) handleUpdateWikiPage(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	b := s.boardFromWiki(w, r)
	if b == nil {
		return
	}
	p, err := s.DB.GetWikiPage(r.PathValue("pageID"))
	if err != nil || p == nil || p.BoardID != b.ID {
		writeErr(w, http.StatusNotFound, "page not found")
		return
	}
	var req struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	title := p.Title
	if req.Title != nil && strings.TrimSpace(*req.Title) != "" {
		title = strings.TrimSpace(*req.Title)
	}
	body := p.Body
	if req.Body != nil {
		body = *req.Body
	}
	u := auth.UserFromContext(r.Context())
	if err := s.DB.UpdateWikiPage(p.ID, title, body, "user:"+u.Username); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	p.Title, p.Body = title, body
	writeJSON(w, http.StatusOK, p)
}

// handleDeleteWikiPage removes a page and its history (admin only).
func (s *Server) handleDeleteWikiPage(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	b := s.boardFromWiki(w, r)
	if b == nil {
		return
	}
	p, err := s.DB.GetWikiPage(r.PathValue("pageID"))
	if err != nil || p == nil || p.BoardID != b.ID {
		writeErr(w, http.StatusNotFound, "page not found")
		return
	}
	if err := s.DB.DeleteWikiPage(p.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- wiki proposals (approval queue) ----------

// handleWikiProposals lists proposals, optionally filtered by ?status=.
func (s *Server) handleWikiProposals(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != model.ProposalPending &&
		status != model.ProposalApproved && status != model.ProposalRejected {
		writeErr(w, http.StatusBadRequest, "bad status")
		return
	}
	proposals, err := s.DB.ListWikiProposals(status, 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if proposals == nil {
		proposals = []*model.WikiProposal{}
	}
	writeJSON(w, http.StatusOK, proposals)
}

// handleApproveWikiProposal applies a pending proposal (admin only).
func (s *Server) handleApproveWikiProposal(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	p, err := s.DB.GetWikiProposal(r.PathValue("id"))
	if err != nil || p == nil {
		writeErr(w, http.StatusNotFound, "proposal not found")
		return
	}
	editedBy := "agent:" + p.CardID
	if p.CardID == "" {
		editedBy = "agent:unknown"
	}
	page, err := s.DB.ApplyWikiProposal(p.ID, editedBy)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot approve: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "page": page})
}

// handleRejectWikiProposal marks a pending proposal rejected (admin only).
func (s *Server) handleRejectWikiProposal(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	if err := s.DB.RejectWikiProposal(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "proposal not found or not pending")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- admin: orphaned volumes ----------

// handleOrphanedVolumes lists Drydock-managed volumes safe to delete: their
// card row is gone or their card is done. Volumes for cards in any other
// column are never listed. When Docker is unreachable the response carries
// {"unavailable": true} instead of an error (admin only).
func (s *Server) handleOrphanedVolumes(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	if s.Orch.Docker == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"unavailable": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	vols, err := s.Orch.OrphanedVolumes(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]bool{"unavailable": true})
		return
	}
	if vols == nil {
		vols = []agents.OrphanedVolume{}
	}
	writeJSON(w, http.StatusOK, vols)
}

// handlePruneVolumes deletes exactly the named volumes, re-verifying at
// prune time that each is still orphaned (a done card reopened between scan
// and prune gets a fresh volume under the same name — deleting it would nuke
// live data). Best-effort per volume; one failure never aborts the rest
// (admin only).
func (s *Server) handlePruneVolumes(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	var req struct {
		Volumes []string `json:"volumes"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(req.Volumes) == 0 {
		writeErr(w, http.StatusBadRequest, "no volumes specified")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	results := s.Orch.PruneVolumes(ctx, req.Volumes)
	if results == nil {
		results = []agents.PruneResult{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ---------- global settings ----------

// handleGetSettings returns global settings (admin only).
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	instructions, _ := s.DB.GetSetting("instructions")
	writeJSON(w, http.StatusOK, map[string]string{"instructions": instructions})
}

// handleUpdateSettings writes global settings (admin only).
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(s, w, r) {
		return
	}
	var req struct {
		Instructions *string `json:"instructions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Instructions != nil {
		if err := s.DB.SetSetting("instructions", strings.TrimSpace(*req.Instructions)); err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
	}
	instructions, _ := s.DB.GetSetting("instructions")
	writeJSON(w, http.StatusOK, map[string]string{"instructions": instructions})
}
