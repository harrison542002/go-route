// Package routing resolves a request into an ordered ladder of targets.

package routing

import (
	"fmt"
	"sort"
	"strings"

	"github.com/harrison542002/go-route/internal/core/domains"
)

// Table is a static alias→ladder map. Policy-based routing will implement
// the same Router contract, producing ladders with ReasonRuleMatch, and
// nothing downstream changes.
type Table struct {
	ladders map[string]domains.Ladder
	aliases string // precomputed for error messages
}

func NewTable(ladders map[string]domains.Ladder) (*Table, error) {
	if len(ladders) == 0 {
		return nil, fmt.Errorf("routing: no models configured")
	}

	names := make([]string, 0, len(ladders))
	for n, l := range ladders {
		if l.IsEmpty() {
			return nil, fmt.Errorf("routing: model %q has an empty ladder", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)

	return &Table{ladders: ladders, aliases: strings.Join(names, ", ")}, nil
}

func (t *Table) Route(facts domains.RequestFacts) (domains.Ladder, error) {
	l, ok := t.ladders[facts.RequestedModel]
	if !ok {
		// Unknown models are rejected rather than passed through: a typo
		// reaching a provider produces a confusing upstream error and an
		// unattributable decision record.
		return domains.Ladder{}, fmt.Errorf(
			"unknown model %q; available: %s", facts.RequestedModel, t.aliases)
	}
	return l, nil
}
