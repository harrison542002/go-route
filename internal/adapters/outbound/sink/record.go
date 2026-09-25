package sink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
)

const recordVersion = 1

var errCorrupt = errors.New("sink: corrupt spool record")
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// envelope is one line of a segment file:
//
//	{"v":1,"crc":3735928559,"rec":{...}}
type envelope struct {
	V   int             `json:"v"`
	CRC uint32          `json:"crc"` // to calculate checksum as json alone cannot tell a whole record from one that lost a page in a crash and happens to still parse
	Rec json.RawMessage `json:"rec"`
}

// spoolRecord is the serialised form of domains.RoutingDecision. It is its own
// type, with explicit tags, so a rename in the domain cannot silently change
// what the spool reads back: files written by one release are read by the next.
type spoolRecord struct {
	ID         uuid.UUID    `json:"id"`
	OccurredAt time.Time    `json:"occurred_at"`
	Tenant     string       `json:"tenant"`
	KeyID      uuid.UUID    `json:"key_id"`
	Request    spoolRequest `json:"request"`
	Ladder     spoolLadder  `json:"ladder"`
	Outcome    spoolOutcome `json:"outcome"`
	Cost       *spoolCost   `json:"cost,omitempty"`
}

type spoolRequest struct {
	RequestedModel string            `json:"requested_model"`
	Stream         bool              `json:"stream"`
	WantsUsage     bool              `json:"wants_usage"`
	Metadata       map[string]string `json:"metadata"`
}

type spoolLadder struct {
	Targets       []domains.TargetRef `json:"targets"`
	ReasonKind    string              `json:"reason_kind"`
	ModelAlias    string              `json:"model_alias,omitempty"`
	RuleName      string              `json:"rule_name,omitempty"`
	PolicyVersion int                 `json:"policy_version,omitempty"`
}

type spoolOutcome struct {
	Status   string            `json:"status"`
	Attempts []domains.Attempt `json:"attempts"`
	Usage    spoolUsage        `json:"usage"`
	TTFTMs   int               `json:"ttft_ms"`
	TotalMs  int               `json:"total_ms"`
}

type spoolUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheWrite int `json:"cache_write"`
	CacheRead  int `json:"cache_read"`
	Reasoning  int `json:"reasoning"`
}

// spoolCost keeps money as integer nanodollars. A float would put rounding
// between the priced record and the ledger row.
type spoolCost struct {
	ActualNanos         int64                    `json:"actual_nanos"`
	PriceTableVersion   string                   `json:"price_table_version"`
	Counterfactuals     []domains.Counterfactual `json:"counterfactuals"`
	UnpricedComparisons []string                 `json:"unpriced_comparisons"`
}

// encodeLine renders one decision as a newline-terminated segment line.
func encodeLine(d domains.RoutingDecision) ([]byte, error) {
	rec, err := json.Marshal(toSpoolRecord(d))
	if err != nil {
		return nil, fmt.Errorf("sink: encode %s: %w", d.ID, err)
	}

	var buf bytes.Buffer
	buf.Grow(len(rec) + 40)
	fmt.Fprintf(&buf, `{"v":%d,"crc":%d,"rec":`, recordVersion, crc32.Checksum(rec, crcTable))
	buf.Write(rec)
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

// decodeLine reverses encodeLine. The line must not include its newline.
func decodeLine(line []byte) (domains.RoutingDecision, error) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return domains.RoutingDecision{}, fmt.Errorf("%w: %v", errCorrupt, err)
	}
	if env.V != recordVersion {
		return domains.RoutingDecision{}, fmt.Errorf("%w: unsupported version %d", errCorrupt, env.V)
	}
	if got := crc32.Checksum(env.Rec, crcTable); got != env.CRC {
		return domains.RoutingDecision{}, fmt.Errorf("%w: checksum %d, want %d", errCorrupt, got, env.CRC)
	}

	var rec spoolRecord
	if err := json.Unmarshal(env.Rec, &rec); err != nil {
		return domains.RoutingDecision{}, fmt.Errorf("%w: %v", errCorrupt, err)
	}
	return rec.toDecision()
}

func toSpoolRecord(d domains.RoutingDecision) spoolRecord {
	rec := spoolRecord{
		ID:         d.ID.UUID(),
		OccurredAt: d.OccurredAt,
		Tenant:     string(d.Tenant),
		KeyID:      d.KeyID,
		Request: spoolRequest{
			RequestedModel: d.Request.RequestedModel,
			Stream:         d.Request.Stream,
			WantsUsage:     d.Request.WantsUsage,
			Metadata:       d.Request.Metadata,
		},
		Ladder: spoolLadder{
			Targets:       d.Ladder.Targets,
			ReasonKind:    string(d.Ladder.Reason.Kind),
			ModelAlias:    d.Ladder.Reason.ModelAlias,
			RuleName:      d.Ladder.Reason.RuleName,
			PolicyVersion: d.Ladder.Reason.PolicyVersion,
		},
		Outcome: spoolOutcome{
			Status:   string(d.Outcome.Status),
			Attempts: d.Outcome.Attempts,
			Usage: spoolUsage{
				Input:      d.Outcome.Usage.Input,
				Output:     d.Outcome.Usage.Output,
				CacheWrite: d.Outcome.Usage.CacheWrite,
				CacheRead:  d.Outcome.Usage.CacheRead,
				Reasoning:  d.Outcome.Usage.Reasoning,
			},
			TTFTMs:  d.Outcome.TTFTMs,
			TotalMs: d.Outcome.TotalMs,
		},
	}

	if d.Cost != nil {
		rec.Cost = &spoolCost{
			ActualNanos:         int64(d.Cost.Actual),
			PriceTableVersion:   d.Cost.PriceTableVersion,
			Counterfactuals:     d.Cost.Counterfactuals,
			UnpricedComparisons: d.Cost.UnpricedComparisons,
		}
	}
	return rec
}

func (r spoolRecord) toDecision() (domains.RoutingDecision, error) {
	if r.ID == uuid.Nil {
		return domains.RoutingDecision{}, fmt.Errorf("%w: record has no id", errCorrupt)
	}
	id, err := domains.ParseDecisionID(r.ID.String())
	if err != nil {
		return domains.RoutingDecision{}, fmt.Errorf("%w: %v", errCorrupt, err)
	}

	d := domains.RoutingDecision{
		ID:         id,
		OccurredAt: r.OccurredAt,
		Tenant:     domains.Tenant(r.Tenant),
		KeyID:      r.KeyID,
		Request: domains.RequestSummary{
			RequestedModel: r.Request.RequestedModel,
			Stream:         r.Request.Stream,
			WantsUsage:     r.Request.WantsUsage,
			Metadata:       r.Request.Metadata,
		},
		Ladder: domains.Ladder{
			Targets: r.Ladder.Targets,
			Reason: domains.Reason{
				Kind:          domains.ReasonKind(r.Ladder.ReasonKind),
				ModelAlias:    r.Ladder.ModelAlias,
				RuleName:      r.Ladder.RuleName,
				PolicyVersion: r.Ladder.PolicyVersion,
			},
		},
		Outcome: domains.Outcome{
			Status:   domains.Status(r.Outcome.Status),
			Attempts: r.Outcome.Attempts,
			Usage: domains.TokenUsage{
				Input:      r.Outcome.Usage.Input,
				Output:     r.Outcome.Usage.Output,
				CacheWrite: r.Outcome.Usage.CacheWrite,
				CacheRead:  r.Outcome.Usage.CacheRead,
				Reasoning:  r.Outcome.Usage.Reasoning,
			},
			TTFTMs:  r.Outcome.TTFTMs,
			TotalMs: r.Outcome.TotalMs,
		},
	}

	if r.Cost != nil {
		d.Cost = &domains.CostBreakdown{
			Actual:              domains.USD(r.Cost.ActualNanos),
			PriceTableVersion:   r.Cost.PriceTableVersion,
			Counterfactuals:     r.Cost.Counterfactuals,
			UnpricedComparisons: r.Cost.UnpricedComparisons,
		}
	}
	return d, nil
}
