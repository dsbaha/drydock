package agents

import (
	"strings"

	"github.com/dsbaha/drydock/internal/model"
)

// DefaultSteps is the fallback plan when no historical template matches.
var DefaultSteps = []string{
	"Understand the task",
	"Plan the approach",
	"Execute the plan",
	"Self-check the result",
}

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "to": true,
	"of": true, "in": true, "on": true, "for": true, "with": true, "is": true,
	"it": true, "this": true, "that": true, "by": true, "from": true,
}

// keywordSet tokenizes s into a set of significant lowercase keywords.
func keywordSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		if len(tok) < 3 || stopwords[tok] {
			continue
		}
		set[tok] = true
	}
	return set
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}

// stepTemplate picks a plan for card by reusing the step labels of the most
// similar completed card from the same source (Jaccard similarity over title
// keywords). Falls back to DefaultSteps when nothing similar exists.
func stepTemplate(card *model.Card, done []*model.Card, labels func(cardID string) ([]string, error)) []string {
	want := keywordSet(card.Title)
	best, bestScore := -1, 0.25
	for i, c := range done {
		if c.ID == card.ID {
			continue
		}
		if s := jaccard(want, keywordSet(c.Title)); s > bestScore {
			best, bestScore = i, s
		}
	}
	if best >= 0 {
		if ls, err := labels(done[best].ID); err == nil && len(ls) > 0 {
			return ls
		}
	}
	return DefaultSteps
}
