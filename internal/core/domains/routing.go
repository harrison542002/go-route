package domains

// TargetRef names one configured place a request can be sent.
type TargetRef struct {
	Name          string // "openai/gpt-5-mini"
	Provider      string
	UpstreamModel string
	Region        string // for residency rules
}

// Ladder is an ordered set of targets to attempt, together with the
// reason it was produced.
type Ladder struct {
	Targets []TargetRef
	Reason  Reason
}

func (l Ladder) IsEmpty() bool { return len(l.Targets) == 0 }

// ReasonKind identifies what produced a ladder. Values are written to the
// decision log, so they are explicit strings rather than iota names: a
// reordering must not silently change the meaning of stored records.
type ReasonKind string

const (
	ReasonModelAlias ReasonKind = "model_alias"
	ReasonRuleMatch  ReasonKind = "rule_match" // policy routing, later
)

// Reason explains why this ladder was chosen. It is the field that makes
// `go-route explain` possible, and the reason routing has a domain type
// at all rather than just returning dialable targets.
type Reason struct {
	Kind          ReasonKind
	ModelAlias    string // set when Kind is ReasonModelAlias
	RuleName      string // set when Kind is ReasonRuleMatch
	PolicyVersion int
}
