package agents

import (
	"strings"
	"testing"

	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/model"
)

func testCard(t *testing.T, database *db.DB, boardID string) *model.Card {
	t.Helper()
	c := &model.Card{
		ID: db.NewID(), BoardID: boardID, Title: "Wiki test card",
		Body: "body", Source: "manual", Column: model.ColWorking,
		AgentRuntime: "echo", CreatedAt: db.Now(), UpdatedAt: db.Now(),
	}
	if err := database.CreateCard(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestWikiProposalEvent: a runtime-emitted wiki proposal lands in the
// approval queue as pending, with a system message on the card.
func TestWikiProposalEvent(t *testing.T) {
	o, database := testOrchestrator(t)
	board, _ := database.EnsureDefaultBoard()
	card := testCard(t, database, board.ID)

	o.Emit(Event{
		Kind:          EvWikiProposal,
		CardID:        card.ID,
		WikiPageID:    "",
		WikiTitle:     "Deploy runbook",
		WikiBody:      "## Deploy\n\nRun ./deploy.sh",
		WikiRationale: "Learned the deploy steps while working this card.",
	})

	proposals, err := database.ListWikiProposals(model.ProposalPending, 10)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("proposals: %v len=%d", err, len(proposals))
	}
	p := proposals[0]
	if p.Title != "Deploy runbook" || p.ProposedBody != "## Deploy\n\nRun ./deploy.sh" ||
		p.BoardID != board.ID || p.CardID != card.ID || p.PageID != "" {
		t.Fatalf("proposal fields wrong: %+v", p)
	}
	msgs, _ := database.ListMessages(card.ID, 10)
	found := false
	for _, m := range msgs {
		if m.From == model.FromSystem && strings.Contains(m.Text, "Proposed wiki edit") &&
			strings.Contains(m.Text, "awaiting approval") {
			found = true
		}
	}
	if !found {
		t.Fatal("no approval system message on card")
	}
}

// TestWikiProposalEventValidation: empty title/body proposals are refused.
func TestWikiProposalEventValidation(t *testing.T) {
	o, database := testOrchestrator(t)
	board, _ := database.EnsureDefaultBoard()
	card := testCard(t, database, board.ID)

	o.Emit(Event{Kind: EvWikiProposal, CardID: card.ID, WikiTitle: "", WikiBody: "x"})
	o.Emit(Event{Kind: EvWikiProposal, CardID: card.ID, WikiTitle: "T", WikiBody: "  "})

	proposals, _ := database.ListWikiProposals("", 10)
	if len(proposals) != 0 {
		t.Fatalf("expected no proposals, got %d", len(proposals))
	}
}

// TestWikiProposalApprove: approving applies the body, writes a revision
// with the agent attribution, and marks the proposal approved.
func TestWikiProposalApprove(t *testing.T) {
	_, database := testOrchestrator(t)
	board, _ := database.EnsureDefaultBoard()

	page := &model.WikiPage{ID: db.NewID(), BoardID: board.ID, Title: "Runbook",
		Body: "old", CreatedAt: db.Now(), UpdatedAt: db.Now()}
	if err := database.CreateWikiPage(page, "user:admin"); err != nil {
		t.Fatal(err)
	}
	prop := &model.WikiProposal{ID: db.NewID(), BoardID: board.ID, PageID: page.ID,
		Title: "Runbook", ProposedBody: "new body", Rationale: "updated",
		CardID: "card1", Status: model.ProposalPending, CreatedAt: db.Now()}
	if err := database.CreateWikiProposal(prop); err != nil {
		t.Fatal(err)
	}

	applied, err := database.ApplyWikiProposal(prop.ID, "agent:card1")
	if err != nil {
		t.Fatal(err)
	}
	if applied.Body != "new body" {
		t.Fatalf("page body not applied: %q", applied.Body)
	}
	got, _ := database.GetWikiProposal(prop.ID)
	if got.Status != model.ProposalApproved || got.DecidedAt == 0 {
		t.Fatalf("proposal not marked approved: %+v", got)
	}
	revs, _ := database.ListWikiRevisions(page.ID, 10)
	if len(revs) != 2 { // initial + approval
		t.Fatalf("expected 2 revisions, got %d", len(revs))
	}
	if revs[0].Body != "new body" || revs[0].EditedBy != "agent:card1" {
		t.Fatalf("revision wrong: %+v", revs[0])
	}
}

// TestWikiProposalApproveNewPage: a proposal with no page ID creates the page.
func TestWikiProposalApproveNewPage(t *testing.T) {
	_, database := testOrchestrator(t)
	board, _ := database.EnsureDefaultBoard()

	prop := &model.WikiProposal{ID: db.NewID(), BoardID: board.ID, PageID: "",
		Title: "New notes", ProposedBody: "fresh page", Rationale: "r",
		Status: model.ProposalPending, CreatedAt: db.Now()}
	if err := database.CreateWikiProposal(prop); err != nil {
		t.Fatal(err)
	}
	page, err := database.ApplyWikiProposal(prop.ID, "agent:unknown")
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "New notes" || page.BoardID != board.ID || page.Body != "fresh page" {
		t.Fatalf("page not created correctly: %+v", page)
	}
}

// TestWikiProposalReject: rejecting marks the proposal without touching the page.
func TestWikiProposalReject(t *testing.T) {
	_, database := testOrchestrator(t)
	board, _ := database.EnsureDefaultBoard()

	page := &model.WikiPage{ID: db.NewID(), BoardID: board.ID, Title: "Runbook",
		Body: "untouched", CreatedAt: db.Now(), UpdatedAt: db.Now()}
	if err := database.CreateWikiPage(page, "user:admin"); err != nil {
		t.Fatal(err)
	}
	prop := &model.WikiProposal{ID: db.NewID(), BoardID: board.ID, PageID: page.ID,
		Title: "Runbook", ProposedBody: "changed", Status: model.ProposalPending, CreatedAt: db.Now()}
	if err := database.CreateWikiProposal(prop); err != nil {
		t.Fatal(err)
	}
	if err := database.RejectWikiProposal(prop.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := database.GetWikiProposal(prop.ID)
	if got.Status != model.ProposalRejected || got.DecidedAt == 0 {
		t.Fatalf("proposal not marked rejected: %+v", got)
	}
	after, _ := database.GetWikiPage(page.ID)
	if after.Body != "untouched" {
		t.Fatalf("page body changed by rejection: %q", after.Body)
	}
	revs, _ := database.ListWikiRevisions(page.ID, 10)
	if len(revs) != 1 {
		t.Fatalf("expected 1 revision, got %d", len(revs))
	}
}

// TestBriefing: the run briefing contains global instructions, board
// instructions, and wiki content.
func TestBriefing(t *testing.T) {
	o, database := testOrchestrator(t)
	if err := database.SetSetting("instructions", "Always cite measurements."); err != nil {
		t.Fatal(err)
	}
	board, _ := database.EnsureDefaultBoard()
	if err := database.UpdateBoard(board.ID, board.Name, "Board rule: no fake behavior."); err != nil {
		t.Fatal(err)
	}
	page := &model.WikiPage{ID: db.NewID(), BoardID: board.ID, Title: "Env notes",
		Body: "The staging DB is reset nightly.", CreatedAt: db.Now(), UpdatedAt: db.Now()}
	if err := database.CreateWikiPage(page, "user:admin"); err != nil {
		t.Fatal(err)
	}

	briefing := o.buildBriefing(board.ID)
	for _, want := range []string{
		"Always cite measurements.",
		"Board rule: no fake behavior.",
		"Env notes",
		"The staging DB is reset nightly.",
	} {
		if !strings.Contains(briefing, want) {
			t.Fatalf("briefing missing %q:\n%s", want, briefing)
		}
	}

	// Empty inputs → empty briefing.
	if err := database.SetSetting("instructions", ""); err != nil {
		t.Fatal(err)
	}
	board2 := &model.Board{ID: db.NewID(), Name: "Empty", CreatedAt: db.Now()}
	if err := database.CreateBoard(board2); err != nil {
		t.Fatal(err)
	}
	if b := o.buildBriefing(board2.ID); b != "" {
		t.Fatalf("expected empty briefing, got %q", b)
	}
}

// TestWikiUserEditWritesRevision: direct admin edits record revisions.
func TestWikiUserEditWritesRevision(t *testing.T) {
	_, database := testOrchestrator(t)
	board, _ := database.EnsureDefaultBoard()

	page := &model.WikiPage{ID: db.NewID(), BoardID: board.ID, Title: "Notes",
		Body: "v1", CreatedAt: db.Now(), UpdatedAt: db.Now()}
	if err := database.CreateWikiPage(page, "user:admin"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateWikiPage(page.ID, "Notes", "v2", "user:bob"); err != nil {
		t.Fatal(err)
	}
	revs, err := database.ListWikiRevisions(page.ID, 10)
	if err != nil || len(revs) != 2 {
		t.Fatalf("revisions: %v len=%d", err, len(revs))
	}
	if revs[0].Body != "v2" || revs[0].EditedBy != "user:bob" {
		t.Fatalf("latest revision wrong: %+v", revs[0])
	}
	if revs[1].EditedBy != "user:admin" {
		t.Fatalf("initial revision attribution wrong: %+v", revs[1])
	}
}
