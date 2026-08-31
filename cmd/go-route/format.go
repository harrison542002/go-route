package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/harrison542002/go-route/internal/core/domains"
)

func newWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func pct(part, whole int64) string {
	if whole == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(part)/float64(whole))
}

func humanCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// signed renders a cost delta from the reader's point of view: a
// negative delta means the chosen target was cheaper, which reads better
// as "+$0.31 more" than as a raw negative.
func signed(d domains.USD) string {
	if d < 0 {
		return "+" + (-d).String()
	}
	return "-" + d.String()
}

func formatMetadata(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic: map order would vary per run

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}

func formatReason(r domains.Reason) string {
	switch r.Kind {
	case domains.ReasonModelAlias:
		return fmt.Sprintf("model alias %q", r.ModelAlias)
	case domains.ReasonRuleMatch:
		return fmt.Sprintf("rule %q (policy v%d)", r.RuleName, r.PolicyVersion)
	default:
		return string(r.Kind)
	}
}

func formatLadder(refs []domains.TargetRef) string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	return strings.Join(names, " → ")
}
