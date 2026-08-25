package domains

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const idPrefix = "dec_"

type DecisionID struct {
	uuid uuid.UUID
}

// NewDecisionID returns a time-ordered decision identifier.
func NewDecisionID() DecisionID {
	u, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("domains: cannot generate decision ID: %v", err))
	}
	return DecisionID{uuid: u}
}

// ParseDecisionID accepts either the prefixed form used in APIs and logs
// or a bare UUID as stored.
func ParseDecisionID(s string) (DecisionID, error) {
	u, err := uuid.Parse(strings.TrimPrefix(s, idPrefix))
	if err != nil {
		return DecisionID{}, fmt.Errorf("domains: invalid decision ID %q: %w", s, err)
	}
	return DecisionID{uuid: u}, nil
}

// String returns the prefixed form for headers, logs, and the CLI.
func (d DecisionID) String() string {
	return idPrefix + d.uuid.String()
}

// UUID returns the bare value for storage (such as Postgresql).
func (d DecisionID) UUID() uuid.UUID { return d.uuid }

func (d DecisionID) IsZero() bool { return d.uuid == uuid.Nil }
