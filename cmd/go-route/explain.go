package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/harrison542002/go-route/internal/adapters/outbound/store/postgresql"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

func explainCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "explain <decision-id>",
		Short: "Show why one request was routed the way it was",
		Long: `Prints the full record for one decision: which targets were
eligible, which was chosen and why, every attempt that was made, what it
cost, and what the alternatives would have cost.

The decision ID appears in the X-Go-Route-Decision-Id response header and
in every decision log line.`,
		Args:    cobra.ExactArgs(1),
		Example: "  go-route explain dec_01a054ff-4eb0-75fd-b67b-44b75a4b88fe",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExplain(cmd.Context(), args[0], jsonOut)
		},
	}

	cmd.Flags().StringVar(&dsnFlag, "dsn", "", "database DSN (overrides the config)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the raw record as JSON")
	return cmd
}

func runExplain(ctx context.Context, rawID string, jsonOut bool) error {
	id, err := domains.ParseDecisionID(rawID)
	if err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dsn, err := storeDSN(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	store, err := postgresql.NewStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	d, err := store.Get(ctx, id)
	if errors.Is(err, ports.ErrDecisionNotFound) {
		return fmt.Errorf("no decision %s; it may have aged out of retention", rawID)
	}
	if err != nil {
		return err
	}

	if jsonOut {
		return printJSON(d)
	}
	printExplain(d)
	return nil
}

func printExplain(d domains.RoutingDecision) {
	fmt.Println()
	fmt.Printf("  %s\n", d.ID)
	fmt.Printf("  %s · tenant %s\n\n",
		d.OccurredAt.UTC().Format("2006-01-02 15:04:05 MST"), d.Tenant)

	section("Request", func(w *tabwriter.Writer) {
		row(w, "model", d.Request.RequestedModel)
		row(w, "stream", yesNo(d.Request.Stream))
		if len(d.Request.Metadata) > 0 {
			row(w, "metadata", formatMetadata(d.Request.Metadata))
		}
	})

	section("Decision", func(w *tabwriter.Writer) {
		row(w, "reason", formatReason(d.Ladder.Reason))
		row(w, "ladder", formatLadder(d.Ladder.Targets))

		chosen := d.Outcome.ChosenTarget()
		if chosen == "" {
			row(w, "chose", "nothing — every target failed")
		} else {
			row(w, "chose", chosen)
		}
		row(w, "status", string(d.Outcome.Status))
	})

	printAttempts(d.Outcome.Attempts)

	if d.Outcome.Usage != (domains.TokenUsage{}) {
		section("Usage", func(w *tabwriter.Writer) {
			input := fmt.Sprintf("%d", d.Outcome.Usage.Input)
			if d.Outcome.Usage.CacheRead > 0 {
				input += fmt.Sprintf("\t(%d cached)", d.Outcome.Usage.CacheRead)
			}
			row(w, "input", input)

			output := fmt.Sprintf("%d", d.Outcome.Usage.Output)
			if d.Outcome.Usage.Reasoning > 0 {
				output += fmt.Sprintf("\t(%d reasoning)", d.Outcome.Usage.Reasoning)
			}
			row(w, "output", output)
		})
	}

	printCost(d)

	section("Timing", func(w *tabwriter.Writer) {
		if d.Outcome.TTFTMs > 0 {
			row(w, "first token", fmt.Sprintf("%dms", d.Outcome.TTFTMs))
		}
		row(w, "total", fmt.Sprintf("%dms", d.Outcome.TotalMs))
	})

	fmt.Println()
}

func printAttempts(attempts []domains.Attempt) {
	if len(attempts) == 0 {
		return
	}

	fmt.Println("  Attempts")
	w := newWriter()
	for i, a := range attempts {
		outcome := "ok"
		detail := ""
		if a.Failure != nil {
			outcome = a.Failure.Kind
			detail = "\t" + a.Failure.Message
		}
		fmt.Fprintf(w, "    %d\t%s\t%s\t%dms%s\n",
			i+1, a.Target, outcome, a.DurationMs, detail)
	}
	_ = w.Flush()
	fmt.Println()
}

func printCost(d domains.RoutingDecision) {
	fmt.Println("  Cost")
	w := newWriter()

	if d.Cost == nil {
		reason := "no rates configured for this target"
		if d.Outcome.ChosenTarget() == "" {
			reason = "nothing was served"
		}
		fmt.Fprintf(w, "    actual\tunpriced\n")
		fmt.Fprintf(w, "    \t%s\n", reason)
		_ = w.Flush()
		fmt.Println()
		return
	}

	fmt.Fprintf(w, "    actual\t%s\t(price table %s)\n",
		d.Cost.Actual, d.Cost.PriceTableVersion)

	for _, cf := range d.Cost.Counterfactuals {
		delta := d.Cost.Actual - cf.Cost
		fmt.Fprintf(w, "    vs %s\t%s\t%s\n", cf.Target, cf.Cost, signed(delta))
	}

	if len(d.Cost.UnpricedComparisons) > 0 {
		fmt.Fprintf(w, "    not compared\t%s\n",
			strings.Join(d.Cost.UnpricedComparisons, ", "))
	}
	_ = w.Flush()

	if len(d.Cost.Counterfactuals) > 0 {
		fmt.Println("                    estimated: actual token counts repriced")
	}
	fmt.Println()
}

func section(title string, body func(*tabwriter.Writer)) {
	fmt.Printf("  %s\n", title)
	w := newWriter()
	body(w)
	_ = w.Flush()
	fmt.Println()
}

func row(w *tabwriter.Writer, label, value string) {
	fmt.Fprintf(w, "    %s\t%s\n", label, value)
}
