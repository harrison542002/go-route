package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/harrison542002/go-route/internal/adapters/outbound/store/postgresql"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

func reportCmd() *cobra.Command {
	var (
		since   string
		until   string
		groupBy string
		tenant  string
		limit   int
		format  string
	)

	cmd := &cobra.Command{
		Use:   "report",
		Short: "Summarise spend over a period",
		Long: `Aggregates decision records into spend, usage, latency and
outcome counts.

--group-by accepts a known grouping (model, target, status, day) or any
metadata key your applications set. "feature" groups by the value of the
x-go-route-feature header.

Comparison columns are estimates: actual token counts repriced against
the alternative target's rates. A different model would have produced a
different number of output tokens.`,
		Example: `  go-route report --since 30d
  go-route report --since 30d --group-by feature
  go-route report --since 2026-08-01 --until 2026-09-01 --group-by target
  go-route report --since 7d --format json | jq`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := buildSpec(since, until, groupBy, tenant, limit)
			if err != nil {
				return err
			}
			return runReport(cmd.Context(), spec, format)
		},
	}

	f := cmd.Flags()
	f.StringVar(&since, "since", "7d", "start of the period: a duration (30d, 24h) or a date")
	f.StringVar(&until, "until", "", "end of the period, exclusive (default: now)")
	f.StringVar(&groupBy, "group-by", "", "model, target, status, day, or a metadata key")
	f.StringVar(&tenant, "tenant", string(domains.DefaultTenant), "tenant to report on")
	f.IntVar(&limit, "limit", ports.DefaultReportLimit, "maximum groups to show")
	f.StringVar(&format, "format", "table", "table, json, or csv")
	f.StringVar(&dsnFlag, "dsn", "", "database DSN (overrides the config)")

	return cmd
}

func buildSpec(since, until, groupBy, tenant string, limit int) (ports.ReportSpec, error) {
	now := time.Now()

	sinceT, err := parseWhen(since, now)
	if err != nil {
		return ports.ReportSpec{}, fmt.Errorf("--since: %w", err)
	}

	untilT := now
	if until != "" {
		if untilT, err = parseWhen(until, now); err != nil {
			return ports.ReportSpec{}, fmt.Errorf("--until: %w", err)
		}
	}

	spec := ports.ReportSpec{
		Tenant: domains.Tenant(tenant),
		Since:  sinceT,
		Until:  untilT,
		Limit:  limit,
	}

	switch groupBy {
	case "":
		spec.GroupBy = ports.GroupByNone
	case "model":
		spec.GroupBy = ports.GroupByModel
	case "target":
		spec.GroupBy = ports.GroupByTarget
	case "status":
		spec.GroupBy = ports.GroupByStatus
	case "day":
		spec.GroupBy = ports.GroupByDay
	default:
		spec.GroupBy = ports.GroupByMetadata
		spec.MetaKey = groupBy
	}

	return spec, spec.Validate()
}

// parseWhen accepts a duration back from now ("30d", "24h") or an
// absolute date ("2026-08-01"). Durations are the common case, so they
// are tried first.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if d, ok := parseDayDuration(s); ok {
		return now.Add(-d), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"%q is neither a duration (30d, 24h) nor a date (2026-08-01)", s)
}

// parseDayDuration handles the "30d" form, which time.ParseDuration does
// not support.
func parseDayDuration(s string) (time.Duration, bool) {
	if !strings.HasSuffix(s, "d") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * 24 * time.Hour, true
}

func runReport(ctx context.Context, spec ports.ReportSpec, format string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dsn, err := storeDSN(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	store, err := postgresql.NewStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	report, err := store.Aggregate(ctx, spec)
	if err != nil {
		return err
	}

	switch format {
	case "json":
		return printJSON(report)
	case "csv":
		return printReportCSV(report, spec)
	case "table":
		printReportTable(report, spec)
		return nil
	default:
		return fmt.Errorf("unknown --format %q: use table, json, or csv", format)
	}
}

func printReportTable(r domains.Report, spec ports.ReportSpec) {
	fmt.Println()
	fmt.Printf("  %s · %s to %s\n\n",
		r.Spec.Tenant,
		r.Spec.Since.UTC().Format("2006-01-02 15:04"),
		r.Spec.Until.UTC().Format("2006-01-02 15:04"))

	if r.Total.Requests == 0 {
		fmt.Println("  No requests in this period.")
		fmt.Println()
		return
	}

	// Comparison targets vary per row, so collect the union and give
	// each a column. Sorted for stable output across runs.
	targets := comparisonTargets(r)

	w := newWriter()
	cols := header(w, spec, targets)

	for _, row := range r.Rows {
		printReportRow(w, spec, row, targets)
	}

	if spec.GroupBy != ports.GroupByNone && len(r.Rows) > 1 {
		rule(w, cols)
		printTotalRow(w, spec, r.Total, targets)
	}
	_ = w.Flush()

	printFootnotes(r, targets)
	fmt.Println()
}

func header(w *tabwriter.Writer, spec ports.ReportSpec, targets []string) int {
	var cols []string

	if spec.GroupBy != ports.GroupByNone {
		cols = append(cols, groupLabel(spec))
	}

	cols = append(cols, "requests", "cost")
	for _, t := range targets {
		cols = append(cols, "vs "+t)
	}
	cols = append(cols, "p95 ttft", "ok", "fail")

	fmt.Fprintf(w, "  %s\n", strings.Join(cols, "\t"))

	return len(cols)
}

func printReportRow(w *tabwriter.Writer, spec ports.ReportSpec, row domains.ReportRow, targets []string) {
	var cells []string
	if spec.GroupBy != ports.GroupByNone {
		cells = append(cells, orDash(row.Key))
	}
	cells = append(cells, humanCount(row.Requests), formatCost(row))

	for _, t := range targets {
		c, ok := row.Comparisons[t]
		switch {
		case !ok:
			cells = append(cells, "—")
		case c.IsPartial(row.Requests):
			// Marked because summing a comparison that covers only part
			// of the traffic understates what the alternative cost.
			cells = append(cells, c.Cost.Auto()+"*")
		default:
			cells = append(cells, c.Cost.Auto())
		}
	}

	cells = append(cells,
		fmt.Sprintf("%dms", row.P95TTFTMs),
		pct(row.OK, row.Requests),
		pct(row.Failed+row.Truncated, row.Requests),
	)

	fmt.Fprintf(w, "  %s\n", strings.Join(cells, "\t"))
}

func printTotalRow(w *tabwriter.Writer, spec ports.ReportSpec, total domains.ReportRow, targets []string) {
	var cells []string
	if spec.GroupBy != ports.GroupByNone {
		cells = append(cells, "total")
	}
	cells = append(cells, humanCount(total.Requests), formatCost(total))

	for _, t := range targets {
		if c, ok := total.Comparisons[t]; ok {
			cells = append(cells, c.Cost.Auto())
		} else {
			cells = append(cells, "—")
		}
	}
	// Percentiles do not aggregate: a p95 of p95s is not a p95. Run the
	// report ungrouped for overall latency.
	cells = append(cells, "", "", "")
	fmt.Fprintf(w, "  %s\n", strings.Join(cells, "\t"))
}

func printFootnotes(r domains.Report, targets []string) {
	fmt.Println()

	if len(targets) > 0 {
		fmt.Println("  Comparisons are estimates: actual token counts repriced against")
		fmt.Println("  the alternative's rates. A different model produces different output.")
	}

	// The coverage disclosure. A total that quietly omits traffic is
	// worse than one that states its own limits.
	if r.Total.Unpriced > 0 {
		fmt.Printf("  %s of requests could not be priced and are excluded from cost.\n",
			pct(r.Total.Unpriced, r.Total.Requests))
	}

	var partial []string
	for _, t := range targets {
		if c, ok := r.Total.Comparisons[t]; ok && c.IsPartial(r.Total.Requests) {
			partial = append(partial, fmt.Sprintf("%s covers %s of requests",
				t, pct(c.Requests, r.Total.Requests)))
		}
	}
	if len(partial) > 0 {
		fmt.Printf("  * partial comparison: %s.\n", strings.Join(partial, "; "))
	}

	if r.TruncatedGroups > 0 {
		fmt.Printf("  %d further group(s) omitted by --limit.\n", r.TruncatedGroups)
	}
}

func comparisonTargets(r domains.Report) []string {
	seen := map[string]struct{}{}
	for _, row := range r.Rows {
		for t := range row.Comparisons {
			seen[t] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func groupLabel(spec ports.ReportSpec) string {
	if spec.GroupBy == ports.GroupByMetadata {
		return spec.MetaKey
	}
	if spec.GroupBy == ports.GroupByNone {
		return ""
	}
	return string(spec.GroupBy)
}

// formatCost shows the cost, marking rows where some traffic could not
// be priced so the figure is not read as complete.
func formatCost(row domains.ReportRow) string {
	s := row.Cost.Auto()
	if row.Unpriced > 0 {
		s += "†"
	}
	return s
}

func rule(w *tabwriter.Writer, cols int) {
	cells := make([]string, cols)
	for i := range cells {
		cells[i] = strings.Repeat("─", 8)
	}
	fmt.Fprintf(w, "  %s\n", strings.Join(cells, "\t"))
}

func printReportCSV(r domains.Report, spec ports.ReportSpec) error {
	targets := comparisonTargets(r)
	w := csv.NewWriter(os.Stdout)
	defer w.Flush()

	head := []string{groupLabel(spec), "requests", "cost_usd", "unpriced"}
	for _, t := range targets {
		head = append(head, "vs_"+t, "vs_"+t+"_coverage")
	}
	head = append(head, "input_tokens", "cached_tokens", "output_tokens",
		"p50_ttft_ms", "p95_ttft_ms", "ok", "failed", "truncated", "disconnected")
	if err := w.Write(head); err != nil {
		return err
	}

	for _, row := range r.Rows {
		rec := []string{
			row.Key,
			strconv.FormatInt(row.Requests, 10),
			strconv.FormatFloat(row.Cost.Dollars(), 'f', 6, 64),
			strconv.FormatInt(row.Unpriced, 10),
		}
		for _, t := range targets {
			c := row.Comparisons[t]
			rec = append(rec,
				strconv.FormatFloat(c.Cost.Dollars(), 'f', 6, 64),
				strconv.FormatInt(c.Requests, 10))
		}
		rec = append(rec,
			strconv.Itoa(row.Usage.Input),
			strconv.Itoa(row.Usage.CacheRead),
			strconv.Itoa(row.Usage.Output),
			strconv.Itoa(row.P50TTFTMs),
			strconv.Itoa(row.P95TTFTMs),
			strconv.FormatInt(row.OK, 10),
			strconv.FormatInt(row.Failed, 10),
			strconv.FormatInt(row.Truncated, 10),
			strconv.FormatInt(row.Disconnected, 10),
		)
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	return w.Error()
}
