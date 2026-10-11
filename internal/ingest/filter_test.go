package ingest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/model"
)

// ---------- ParseVerdict ----------

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantAccept bool
		wantReason string
		wantErr    bool
	}{
		{"accept", `{"accept": true, "reason": "matches the rules"}`, true, "matches the rules", false},
		{"reject", `{"accept": false, "reason": "spam"}`, false, "spam", false},
		{"code fence", "```json\n{\"accept\": true, \"reason\": \"ok\"}\n```", true, "ok", false},
		{"string yes", `{"accept": "yes", "reason": "fine"}`, true, "fine", false},
		{"string no", `{"accept": "no", "reason": "junk"}`, false, "junk", false},
		{"surrounding prose", "Sure. {\"accept\": false, \"reason\": \"ad\"} Done.", false, "ad", false},
		{"missing reason", `{"accept": true}`, true, "no reason given", false},
		{"garbage", "definitely not json", false, "", true},
		{"bad accept", `{"accept": [1], "reason": "x"}`, false, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			accept, reason, err := ParseVerdict(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if accept != c.wantAccept || reason != c.wantReason {
				t.Fatalf("got (%v, %q), want (%v, %q)", accept, reason, c.wantAccept, c.wantReason)
			}
		})
	}
}

// ---------- test helpers ----------

type stubClassifier struct {
	verdict Verdict
	err     error
}

func (s *stubClassifier) Classify(ctx context.Context, instructions, title, body string) (Verdict, error) {
	return s.verdict, s.err
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func testService(t *testing.T, database *db.DB) (*Service, *[]*model.Card) {
	t.Helper()
	var cards []*model.Card
	sink := func(boardID, title, body, source string) (*model.Card, error) {
		c := &model.Card{ID: db.NewID(), BoardID: boardID, Title: title, Body: body, Source: source}
		cards = append(cards, c)
		return c, nil
	}
	return New(database, sink), &cards
}

func rssServer(t *testing.T, itemsXML string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Test</title>%s</channel></rss>`, itemsXML)
	}))
}

func testFeed(t *testing.T, database *db.DB, url string, filterEnabled bool) *model.Feed {
	t.Helper()
	b, err := database.EnsureDefaultBoard()
	if err != nil {
		t.Fatal(err)
	}
	f := &model.Feed{
		ID: db.NewID(), Name: "testfeed", URL: url, BoardID: b.ID, IntervalSec: 60,
		FilterEnabled: filterEnabled, FilterInstructions: "Accept only posts about Go; reject everything else.",
		CreatedAt: db.Now(),
	}
	if err := database.CreateFeed(f); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetFeed(f.ID)
	if err != nil || got == nil {
		t.Fatalf("get feed: %v", err)
	}
	return got
}

const rssItems = `<item><guid>a1</guid><title>Go 1.26 released</title><description>New Go version.</description><link>http://x/1</link></item>
<item><guid>a2</guid><title>Buy cheap watches</title><description>Spam spam.</description><link>http://x/2</link></item>`

// ---------- Filter.Evaluate ----------

func TestEvaluateNilFilterFailOpen(t *testing.T) {
	var f *Filter
	accept, reason := f.Evaluate("rss", "rss:x", "t", "b", "rules")
	if !accept || !strings.Contains(reason, "filter unavailable") {
		t.Fatalf("got (%v, %q), want fail-open accept", accept, reason)
	}
}

func TestEvaluateClassifierErrorFailOpen(t *testing.T) {
	f := &Filter{Classify: &stubClassifier{err: fmt.Errorf("boom")}}
	accept, reason := f.Evaluate("rss", "rss:x", "t", "b", "rules")
	if !accept || !strings.Contains(reason, "filter unavailable: boom") {
		t.Fatalf("got (%v, %q), want fail-open accept", accept, reason)
	}
}

// ---------- LLMClassifier fail-open ----------

func TestLLMClassifierNoProviderFailOpen(t *testing.T) {
	database := testDB(t)
	c := &LLMClassifier{DB: database}
	v, err := c.Classify(context.Background(), "rules", "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Accept || !strings.Contains(v.Reason, "no LLM provider configured") {
		t.Fatalf("got %+v, want fail-open accept", v)
	}
}

func chatStub(t *testing.T, status int, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, content)
	}))
}

func addFakeProvider(t *testing.T, database *db.DB, endpoint string) {
	t.Helper()
	p := &model.LLMProvider{
		ID: db.NewID(), Name: "fake", ProviderType: "openai",
		Endpoint: endpoint, APIKey: "k", Model: "m",
		CreatedAt: db.Now(), UpdatedAt: db.Now(),
	}
	if err := database.CreateProvider(p); err != nil {
		t.Fatal(err)
	}
}

func TestLLMClassifierErrorFailOpen(t *testing.T) {
	database := testDB(t)
	srv := chatStub(t, 500, "boom")
	defer srv.Close()
	addFakeProvider(t, database, srv.URL)
	c := &LLMClassifier{DB: database}
	v, err := c.Classify(context.Background(), "rules", "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Accept || !strings.HasPrefix(v.Reason, "filter unavailable:") {
		t.Fatalf("got %+v, want fail-open accept", v)
	}
}

func TestLLMClassifierVerdict(t *testing.T) {
	database := testDB(t)
	srv := chatStub(t, 200, `{"choices":[{"message":{"content":"{\"accept\": false, \"reason\": \"looks like spam\"}"}}]}`)
	defer srv.Close()
	addFakeProvider(t, database, srv.URL)
	c := &LLMClassifier{DB: database}
	v, err := c.Classify(context.Background(), "rules", "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if v.Accept || v.Reason != "looks like spam" {
		t.Fatalf("got %+v, want reject", v)
	}
}

// ---------- RSS paths ----------

func TestRSSFilterDisabledPassthrough(t *testing.T) {
	database := testDB(t)
	srv := rssServer(t, rssItems)
	defer srv.Close()
	svc, cards := testService(t, database)
	// Filter wired but the feed has it disabled: everything auto-starts.
	svc.Filter = &Filter{Classify: &stubClassifier{verdict: Verdict{Accept: false, Reason: "nope"}}}
	feed := testFeed(t, database, srv.URL, false)
	n, err := svc.PollFeed(feed)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(*cards) != 2 {
		t.Fatalf("got %d created / %d cards, want 2/2", n, len(*cards))
	}
	if items, _ := database.ListRejectedItems(10, 0); len(items) != 0 {
		t.Fatalf("expected no rejected items, got %d", len(items))
	}
}

func TestRSSRejectPath(t *testing.T) {
	database := testDB(t)
	srv := rssServer(t, rssItems)
	defer srv.Close()
	svc, cards := testService(t, database)
	svc.Filter = &Filter{Classify: &stubClassifier{verdict: Verdict{Accept: false, Reason: "not about Go"}}}
	feed := testFeed(t, database, srv.URL, true)
	n, err := svc.PollFeed(feed)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(*cards) != 0 {
		t.Fatalf("got %d created / %d cards, want 0/0", n, len(*cards))
	}
	items, err := database.ListRejectedItems(10, 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("got %d rejected items (err %v), want 2", len(items), err)
	}
	if items[0].Kind != "rss" || items[0].Source != "rss:testfeed" || items[0].Reason != "not about Go" {
		t.Fatalf("unexpected rejected item: %+v", items[0])
	}
	// Second poll must not re-triage (guids marked seen on reject).
	n, _ = svc.PollFeed(feed)
	if n != 0 {
		t.Fatalf("second poll created %d, want 0", n)
	}
}

func TestRSSAcceptPath(t *testing.T) {
	database := testDB(t)
	srv := rssServer(t, rssItems)
	defer srv.Close()
	svc, cards := testService(t, database)
	svc.Filter = &Filter{Classify: &stubClassifier{verdict: Verdict{Accept: true, Reason: "matches"}}}
	feed := testFeed(t, database, srv.URL, true)
	n, err := svc.PollFeed(feed)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(*cards) != 2 {
		t.Fatalf("got %d created / %d cards, want 2/2", n, len(*cards))
	}
}

// ---------- webhook paths ----------

func TestWebhookRejectPath(t *testing.T) {
	database := testDB(t)
	svc, cards := testService(t, database)
	svc.Filter = &Filter{Classify: &stubClassifier{verdict: Verdict{Accept: false, Reason: "spammy"}}}
	if err := database.SetWebhookFilter("webhook:gh", true, "Reject spam."); err != nil {
		t.Fatal(err)
	}
	out, err := svc.Webhook("gh", map[string]any{"title": "Free money!!!"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Accepted || out.Card != nil {
		t.Fatalf("got %+v, want rejected", out)
	}
	if len(*cards) != 0 {
		t.Fatalf("sink called %d times, want 0", len(*cards))
	}
	items, _ := database.ListRejectedItems(10, 0)
	if len(items) != 1 || items[0].Kind != "webhook" || items[0].Source != "webhook:gh" || items[0].Reason != "spammy" {
		t.Fatalf("unexpected rejected items: %+v", items)
	}
}

func TestWebhookDisabledPassthrough(t *testing.T) {
	database := testDB(t)
	svc, cards := testService(t, database)
	svc.Filter = &Filter{Classify: &stubClassifier{verdict: Verdict{Accept: false, Reason: "nope"}}}
	// No filter row yet: first sight auto-creates it disabled.
	out, err := svc.Webhook("gh", map[string]any{"title": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Accepted || out.Card == nil || len(*cards) != 1 {
		t.Fatalf("got %+v / %d cards, want accepted card", out, len(*cards))
	}
}

// ---------- rejected items ----------

func TestPruneRejectedItems(t *testing.T) {
	database := testDB(t)
	old := &model.RejectedItem{ID: db.NewID(), Kind: "rss", Source: "s", Title: "old",
		CreatedAt: db.Now() - 31*86400}
	new := &model.RejectedItem{ID: db.NewID(), Kind: "rss", Source: "s", Title: "new",
		CreatedAt: db.Now()}
	if err := database.CreateRejectedItem(old); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateRejectedItem(new); err != nil {
		t.Fatal(err)
	}
	if err := database.PruneRejectedItems(30 * 86400); err != nil {
		t.Fatal(err)
	}
	items, _ := database.ListRejectedItems(10, 0)
	if len(items) != 1 || items[0].Title != "new" {
		t.Fatalf("got %+v, want only the new item", items)
	}
	if got, _ := database.GetRejectedItem(old.ID); got != nil {
		t.Fatal("old item not pruned")
	}
	if err := database.DeleteRejectedItem(new.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := database.GetRejectedItem(new.ID); got != nil {
		t.Fatal("item not deleted")
	}
}
