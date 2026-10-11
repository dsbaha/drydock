// Package ingest turns external data sources into kanban cards.
//
// A Connector is anything that can produce cards: RSS feeds, webhooks,
// email, chat, … The Service owns the configured connectors and hands new
// items to the Orchestrator, which creates the card in the inbox and
// auto-starts it. Dedupe is per-connector (RSS uses the item guid).
package ingest

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/flows"
	"github.com/dsbaha/drydock/internal/model"
)

// CardSink receives new cards from connectors. kind is "rss" | "webhook";
// the sink routes the item through the board's flow, which may create the
// card, decline it at a flow-level filter, or fail.
type CardSink func(boardID, kind, title, body, source string) (*flows.Result, error)

// Connector is a configured data source. Poll-based connectors implement
// Poll; push-based ones (webhooks) are driven by the server instead.
type Connector interface {
	Name() string
}

// Service owns connectors and fans new items into cards.
type Service struct {
	DB   *db.DB
	Sink CardSink

	// Filter gates inbound items behind LLM triage when the source has
	// filtering enabled. Nil disables filtering (fail-open).
	Filter *Filter

	httpClient *http.Client
}

// New builds the ingest service.
func New(database *db.DB, sink CardSink) *Service {
	return &Service{
		DB:         database,
		Sink:       sink,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Outcome describes what happened to one inbound item.
type Outcome struct {
	Card     *model.Card // nil when rejected or filtered out
	Accepted bool
	Reason   string // filter verdict reason ("" when no filtering applied)
}

// Webhook handles POST /api/ingest/webhook/:source: any JSON body with a
// title becomes a card in the default board's inbox, auto-started — unless
// the source has intake filtering enabled, in which case the LLM triage
// decides. Rejected items are recorded for later review, not dropped.
func (s *Service) Webhook(source string, payload map[string]any) (Outcome, error) {
	title, _ := payload["title"].(string)
	if title == "" {
		title, _ = payload["text"].(string)
	}
	if title == "" {
		title = "Webhook event from " + source
	}
	body, _ := payload["body"].(string)
	if body == "" {
		if u, ok := payload["url"].(string); ok && u != "" {
			body = u
		}
	}
	board, err := s.DB.EnsureDefaultBoard()
	if err != nil {
		return Outcome{}, err
	}
	fullSource := "webhook:" + source
	if wf, err := s.DB.EnsureWebhookFilter(fullSource); err == nil && wf.FilterEnabled && s.Filter != nil {
		accept, reason := s.Filter.Evaluate("webhook", fullSource, title, body, wf.FilterInstructions)
		if !accept {
			raw, _ := json.Marshal(payload)
			_ = s.DB.CreateRejectedItem(&model.RejectedItem{
				ID: db.NewID(), Kind: "webhook", Source: fullSource,
				Title: title, Reason: reason, Payload: truncateRunes(string(raw), 4000),
				BoardID: board.ID, CreatedAt: db.Now(),
			})
			log.Printf("drydock: webhook %q rejected item %q: %s", source, title, reason)
			return Outcome{Accepted: false, Reason: reason}, nil
		}
		return s.acceptCard(board.ID, "webhook", title, body, fullSource, reason)
	}
	res, err := s.Sink(board.ID, "webhook", title, body, fullSource)
	if err != nil {
		return Outcome{}, err
	}
	if res.Rejected {
		return Outcome{Accepted: false, Reason: res.Reason}, nil
	}
	return Outcome{Card: res.Card, Accepted: true}, nil
}

// acceptCard routes an intake-filtered item through the board's flow and
// records the filter verdict. A fail-open verdict ("filter unavailable: ...")
// is logged as an event so the admin UI can hint that the filter was not
// actually consulted.
func (s *Service) acceptCard(boardID, kind, title, body, source, reason string) (Outcome, error) {
	res, err := s.Sink(boardID, kind, title, body, source)
	if err != nil {
		return Outcome{}, err
	}
	if res.Rejected {
		return Outcome{Accepted: false, Reason: res.Reason}, nil
	}
	card := res.Card
	r := reason
	if r == "" {
		r = res.Reason // flow-level fail-open verdict, if any
	}
	if card != nil && strings.HasPrefix(r, unavailablePrefix) {
		_ = s.DB.LogEvent(card.ID, "filter", "accepted ("+r+")")
	}
	return Outcome{Card: card, Accepted: true, Reason: reason}, nil
}

// ---------- RSS ----------

// rssItem is the subset of RSS 2.0 we care about.
type rssItem struct {
	GUID        string `xml:"guid"`
	Link        string `xml:"link"`
	Title       string `xml:"title"`
	Description string `xml:"description"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssFeed struct {
	Channel rssChannel `xml:"channel"`
}

// FetchFeed downloads and parses an RSS 2.0 feed.
func (s *Service) FetchFeed(url string) ([]rssItem, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Drydock/1.0 (+rss-ingest)")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch feed: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var feed rssFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	return feed.Channel.Items, nil
}

// PollFeed fetches one configured feed and creates a card per unseen item.
func (s *Service) PollFeed(feed *model.Feed) (int, error) {
	items, err := s.FetchFeed(feed.URL)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, it := range items {
		guid := strings.TrimSpace(it.GUID)
		if guid == "" {
			guid = strings.TrimSpace(it.Link)
		}
		if guid == "" {
			guid = strings.TrimSpace(it.Title)
		}
		seen, err := s.DB.FeedSeenGUID(feed.ID, guid)
		if err != nil || seen {
			continue
		}
		body := strings.TrimSpace(it.Description)
		if it.Link != "" {
			if body != "" {
				body += "\n\n"
			}
			body += it.Link
		}
		title := strings.TrimSpace(it.Title)
		source := "rss:" + feed.Name
		if feed.FilterEnabled && s.Filter != nil {
			accept, reason := s.Filter.Evaluate("rss", source, title, body, feed.FilterInstructions)
			_ = s.DB.FeedMarkSeen(feed.ID, guid) // seen either way: never re-triage
			if !accept {
				_ = s.DB.CreateRejectedItem(&model.RejectedItem{
					ID: db.NewID(), Kind: "rss", Source: source,
					Title: title, Reason: reason, Payload: truncateRunes(body, 4000),
					BoardID: feed.BoardID, CreatedAt: db.Now(),
				})
				log.Printf("drydock: feed %q rejected item %q: %s", feed.Name, title, reason)
				continue
			}
			if _, err := s.acceptCard(feed.BoardID, "rss", title, body, source, reason); err != nil {
				log.Printf("drydock: rss card create: %v", err)
				continue
			}
			created++
			continue
		}
		res, err := s.Sink(feed.BoardID, "rss", title, body, source)
		if err != nil {
			log.Printf("drydock: rss card create: %v", err)
			continue
		}
		_ = s.DB.FeedMarkSeen(feed.ID, guid)
		if res.Rejected {
			continue // flow-level filter declined it; recorded in rejected_items
		}
		created++
	}
	_ = s.DB.TouchFeed(feed.ID, db.Now())
	if created > 0 {
		_ = s.DB.SetFeedLastItem(feed.ID, db.Now())
	}
	return created, nil
}

// PollLoop periodically polls every configured feed whose interval has
// elapsed. It runs until ctx is cancelled.
func (s *Service) PollLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollDue()
		}
	}
}

func (s *Service) pollDue() {
	// Opportunistic housekeeping: rejected items older than 30 days expire.
	_ = s.DB.PruneRejectedItems(30 * 86400)
	feeds, err := s.DB.ListFeeds()
	if err != nil {
		return
	}
	now := db.Now()
	for _, f := range feeds {
		interval := f.IntervalSec
		if interval < 60 {
			interval = 60
		}
		if now-f.LastPolledAt < int64(interval) {
			continue
		}
		n, err := s.PollFeed(f)
		if err != nil {
			log.Printf("drydock: poll feed %q: %v", f.Name, err)
			_ = s.DB.TouchFeed(f.ID, now) // don't hammer a broken feed
			continue
		}
		if n > 0 {
			log.Printf("drydock: feed %q produced %d card(s)", f.Name, n)
		}
	}
}
