// Package ingest turns external data sources into kanban cards.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/llm"
)

// Verdict is the intake filter's decision for one inbound item.
type Verdict struct {
	Accept bool
	Reason string
}

// Classifier decides whether one inbound item should become a card.
// Implementations must be safe for concurrent use.
type Classifier interface {
	Classify(ctx context.Context, instructions, title, body string) (Verdict, error)
}

// Filter gates inbound items behind an LLM triage step. It is fail-open:
// anything that goes wrong (no classifier, no provider, LLM error,
// unparseable verdict) accepts the item with a "filter unavailable" reason
// rather than silently dropping inbound work.
type Filter struct {
	Classify Classifier
}

// Evaluate runs the classifier for one item. Returns accept=true unless the
// classifier explicitly rejects.
func (f *Filter) Evaluate(kind, source, title, body, instructions string) (accept bool, reason string) {
	if f == nil || f.Classify == nil {
		return true, "filter unavailable: no classifier configured"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	v, err := f.Classify.Classify(ctx, instructions, title, body)
	if err != nil {
		return true, "filter unavailable: " + err.Error()
	}
	return v.Accept, v.Reason
}

// unavailablePrefix marks fail-open accepts so the admin UI can hint that
// the filter was not actually consulted.
const unavailablePrefix = "filter unavailable:"

// LLMClassifier implements Classifier against the admin-configured LLM
// providers (the first one listed). Fail-open: no provider, a provider
// error, or an unparseable verdict all accept the item with a
// "filter unavailable: ..." reason.
type LLMClassifier struct {
	DB *db.DB
}

// FilterSystemPrompt builds the classification prompt from the source's
// intake instructions. The model must reply with ONLY a JSON object:
// {"accept": true|false, "reason": "<one short sentence>"}.
func FilterSystemPrompt(instructions string) string {
	var sb strings.Builder
	sb.WriteString("You are an intake filter for a task-management system. ")
	sb.WriteString("Decide whether the inbound item below should become a task card for an AI agent to work on.\n\n")
	sb.WriteString("Intake rules for this source:\n")
	sb.WriteString(strings.TrimSpace(instructions))
	sb.WriteString("\n\nRespond with ONLY a JSON object and no other text:\n")
	sb.WriteString(`{"accept": true|false, "reason": "<one short sentence>"}`)
	sb.WriteString("\n\nAccept only items that clearly match the intake rules. ")
	sb.WriteString("When in doubt, reject with a reason.")
	return sb.String()
}

func (c *LLMClassifier) Classify(ctx context.Context, instructions, title, body string) (Verdict, error) {
	failOpen := func(format string, args ...any) (Verdict, error) {
		return Verdict{Accept: true, Reason: unavailablePrefix + " " + fmt.Sprintf(format, args...)}, nil
	}
	providers, err := c.DB.ListProviders()
	if err != nil {
		return failOpen("provider lookup failed: %v", err)
	}
	if len(providers) == 0 {
		return failOpen("no LLM provider configured")
	}
	ptype, endpoint, apiKey, model, err := c.DB.GetProviderKey(providers[0].ID)
	if err != nil {
		return failOpen("provider lookup failed: %v", err)
	}
	if apiKey == "" {
		return failOpen("provider has no API key")
	}
	user := "Title: " + strings.TrimSpace(title) + "\n\n" + truncateRunes(strings.TrimSpace(body), 4000)
	resp, err := llm.Chat(ctx, ptype, endpoint, apiKey, model,
		FilterSystemPrompt(instructions),
		[]llm.Message{{Role: "user", Content: user}}, "")
	if err != nil {
		return failOpen("%v", err)
	}
	accept, reason, perr := ParseVerdict(resp)
	if perr != nil {
		return failOpen("could not parse classifier output: %v", perr)
	}
	return Verdict{Accept: accept, Reason: reason}, nil
}

// ParseVerdict extracts {accept, reason} from a classifier reply, tolerating
// markdown code fences and surrounding prose. accept also accepts the strings
// "true"/"yes"/"1".
func ParseVerdict(raw string) (bool, string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return false, "", fmt.Errorf("no JSON object in classifier output")
	}
	var v struct {
		Accept json.RawMessage `json:"accept"`
		Reason string          `json:"reason"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return false, "", fmt.Errorf("invalid verdict JSON: %w", err)
	}
	accept := false
	if len(v.Accept) > 0 {
		var b bool
		if err := json.Unmarshal(v.Accept, &b); err == nil {
			accept = b
		} else {
			var str string
			if err := json.Unmarshal(v.Accept, &str); err != nil {
				return false, "", fmt.Errorf("bad accept value: %s", truncateRunes(string(v.Accept), 40))
			}
			switch strings.ToLower(strings.TrimSpace(str)) {
			case "true", "yes", "1":
				accept = true
			}
		}
	}
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		reason = "no reason given"
	}
	return accept, reason, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
