//go:build integration

package integration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/adapters/outbound/store/postgresql"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

var _ = Describe("Store", func() {
	var (
		store  *postgresql.Store
		writer *sink.PostgresWriter
	)

	BeforeEach(func() {
		truncate()

		c, cancel := ctx()
		defer cancel()

		var err error
		writer, err = sink.NewPostgresWriter(c, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(writer.Close)

		store, err = postgresql.NewStore(c, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(store.Close)
	})

	write := func(batch ...domains.RoutingDecision) {
		c, cancel := ctx()
		defer cancel()
		Expect(writer.Write(c, batch)).To(Succeed())
	}

	report := func(spec ports.ReportSpec) domains.Report {
		c, cancel := ctx()
		defer cancel()

		if spec.Since.IsZero() {
			spec.Since = testTime.Add(-24 * time.Hour)
		}
		if spec.Until.IsZero() {
			spec.Until = testTime.Add(24 * time.Hour)
		}
		if spec.Tenant == "" {
			spec.Tenant = domains.DefaultTenant
		}

		r, err := store.Aggregate(c, spec)
		Expect(err).NotTo(HaveOccurred())
		return r
	}

	Describe("Get", func() {
		It("round-trips a full decision", func() {
			d := newDecision()
			write(d)

			c, cancel := ctx()
			defer cancel()

			got, err := store.Get(c, d.ID)
			Expect(err).NotTo(HaveOccurred())

			Expect(got.ID).To(Equal(d.ID))
			Expect(got.Tenant).To(Equal(d.Tenant))
			Expect(got.Request.RequestedModel).To(Equal("chat"))
			Expect(got.Request.Metadata).To(HaveKeyWithValue("feature", "auto-tag"))
			Expect(got.Outcome.Status).To(Equal(domains.StatusOK))
			Expect(got.Outcome.ChosenTarget()).To(Equal("openai/gpt-5-mini"))
			Expect(got.Outcome.Usage.CacheRead).To(Equal(20))
			Expect(got.Ladder.Reason.Kind).To(Equal(domains.ReasonModelAlias))
			Expect(got.Ladder.Reason.ModelAlias).To(Equal("chat"))
		})

		It("preserves the cost breakdown", func() {
			d := newDecision()
			write(d)

			c, cancel := ctx()
			defer cancel()

			got, err := store.Get(c, d.ID)
			Expect(err).NotTo(HaveOccurred())

			Expect(got.Cost).NotTo(BeNil())
			Expect(got.Cost.Actual).To(Equal(domains.USD(120_500)))
			Expect(got.Cost.PriceTableVersion).To(Equal("2026-08-01"))
			Expect(got.Cost.Counterfactuals).To(HaveLen(1))
			Expect(got.Cost.Counterfactuals[0].Target).To(Equal("openai/gpt-5"))
		})

		It("keeps an unpriced decision unpriced", func() {
			d := newDecision(unpriced())
			write(d)

			c, cancel := ctx()
			defer cancel()

			got, err := store.Get(c, d.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Cost).To(BeNil())
		})

		It("reports a missing decision distinctly", func() {
			c, cancel := ctx()
			defer cancel()

			_, err := store.Get(c, domains.NewDecisionID())
			Expect(err).To(MatchError(ports.ErrDecisionNotFound))
		})
	})

	Describe("Aggregate", func() {
		Describe("validation", func() {
			It("rejects a reversed range", func() {
				c, cancel := ctx()
				defer cancel()

				_, err := store.Aggregate(c, ports.ReportSpec{
					Tenant: domains.DefaultTenant,
					Since:  testTime,
					Until:  testTime.Add(-time.Hour),
				})
				Expect(err).To(MatchError(ContainSubstring("must be before")))
			})

			It("rejects grouping by metadata with no key", func() {
				c, cancel := ctx()
				defer cancel()

				_, err := store.Aggregate(c, ports.ReportSpec{
					Tenant:  domains.DefaultTenant,
					Since:   testTime.Add(-time.Hour),
					Until:   testTime,
					GroupBy: ports.GroupByMetadata,
				})
				Expect(err).To(HaveOccurred())
			})
		})

		It("returns an empty report rather than an error", func() {
			r := report(ports.ReportSpec{})
			Expect(r.Rows).To(BeEmpty())
			Expect(r.Total.Requests).To(BeZero())
		})

		It("sums cost and usage", func() {
			write(newDecision(), newDecision(), newDecision())

			r := report(ports.ReportSpec{})

			Expect(r.Total.Requests).To(Equal(int64(3)))
			Expect(r.Total.Cost).To(Equal(domains.USD(361_500)))
			Expect(r.Total.Usage.Input).To(Equal(240))
			Expect(r.Total.Usage.CacheRead).To(Equal(60))
		})

		// The distinction the nullable column exists for, surviving all
		// the way to the report.
		It("excludes unpriced rows from cost but counts them", func() {
			write(newDecision(), newDecision(unpriced()), newDecision(unpriced()))

			r := report(ports.ReportSpec{})

			Expect(r.Total.Requests).To(Equal(int64(3)))
			Expect(r.Total.Cost).To(Equal(domains.USD(120_500)), "only the priced row contributes")
			Expect(r.Total.Unpriced).To(Equal(int64(2)))
			Expect(r.Total.Coverage()).To(BeNumerically("~", 1.0/3.0, 0.01))
		})

		It("counts outcomes separately", func() {
			write(newDecision(), newDecision(), newDecision(exhausted()))

			r := report(ports.ReportSpec{})

			Expect(r.Total.OK).To(Equal(int64(2)))
			Expect(r.Total.Failed).To(Equal(int64(1)))
		})

		Describe("grouping", func() {
			BeforeEach(func() {
				write(
					newDecision(withMetadata(map[string]string{"feature": "auto-tag"})),
					newDecision(withMetadata(map[string]string{"feature": "auto-tag"})),
					newDecision(withMetadata(map[string]string{"feature": "chat-widget"})),
					newDecision(withMetadata(map[string]string{})),
					newDecision(exhausted(), withMetadata(map[string]string{"team": "platform"})),
				)
			})

			It("groups by a metadata key", func() {
				r := report(ports.ReportSpec{GroupBy: ports.GroupByMetadata, MetaKey: "feature"})

				byKey := map[string]int64{}
				for _, row := range r.Rows {
					byKey[row.Key] = row.Requests
				}
				Expect(byKey).To(HaveKeyWithValue("auto-tag", int64(2)))
				Expect(byKey).To(HaveKeyWithValue("chat-widget", int64(1)))
				Expect(byKey).To(HaveKeyWithValue("(unset)", int64(2)),
					"requests without the key must appear, not vanish")
			})

			It("groups by target, keeping unserved requests", func() {
				r := report(ports.ReportSpec{GroupBy: ports.GroupByTarget})

				byKey := map[string]int64{}
				for _, row := range r.Rows {
					byKey[row.Key] = row.Requests
				}
				Expect(byKey).To(HaveKeyWithValue("openai/gpt-5-mini", int64(4)))
				Expect(byKey).To(HaveKeyWithValue("(unserved)", int64(1)),
					"exhausted requests consumed latency and belong in the report")
			})

			It("groups by status", func() {
				r := report(ports.ReportSpec{GroupBy: ports.GroupByStatus})
				Expect(r.Rows).To(HaveLen(2))
			})

			It("totals match the ungrouped report", func() {
				grouped := report(ports.ReportSpec{GroupBy: ports.GroupByMetadata, MetaKey: "feature"})
				flat := report(ports.ReportSpec{})

				Expect(grouped.Total.Requests).To(Equal(flat.Total.Requests))
				Expect(grouped.Total.Cost).To(Equal(flat.Total.Cost))
				Expect(grouped.Total.Unpriced).To(Equal(flat.Total.Unpriced))
			})
		})

		// The second query joins jsonb_array_elements. If it were folded
		// into the main aggregate, every other figure would be multiplied
		// by the number of counterfactuals per row.
		Describe("counterfactuals", func() {
			It("does not multiply the main aggregates", func() {
				write(newDecision(), newDecision())

				r := report(ports.ReportSpec{})

				Expect(r.Total.Requests).To(Equal(int64(2)),
					"the counterfactual join must not inflate the request count")
				Expect(r.Total.Cost).To(Equal(domains.USD(241_000)))
			})

			It("sums per target", func() {
				write(newDecision(), newDecision())

				r := report(ports.ReportSpec{})

				c, ok := r.Total.Comparisons["openai/gpt-5"]
				Expect(ok).To(BeTrue())
				Expect(c.Cost).To(Equal(domains.USD(1_205_000)))
				Expect(c.Requests).To(Equal(int64(2)))
			})

			// A comparison added mid-period covers only part of the
			// traffic; presenting the sum without that caveat would
			// understate what the alternative cost.
			It("reports partial coverage", func() {
				withoutComparison := func(d *domains.RoutingDecision) {
					d.Cost.Counterfactuals = nil
				}
				write(newDecision(), newDecision(withoutComparison))

				r := report(ports.ReportSpec{})

				c := r.Total.Comparisons["openai/gpt-5"]
				Expect(c.Requests).To(Equal(int64(1)))
				Expect(c.IsPartial(r.Total.Requests)).To(BeTrue())
			})
		})

		Describe("latency", func() {
			// An exhausted request has ttft_ms = 0 by construction.
			// Including those zeros would make latency look better the
			// more failures a group has.
			It("excludes unserved requests from percentiles", func() {
				write(newDecision(), newDecision(), newDecision(exhausted()))

				r := report(ports.ReportSpec{})

				Expect(r.Total.Requests).To(Equal(int64(3)))
				Expect(r.Rows[0].P95TTFTMs).To(BeNumerically(">", 0))
			})
		})

		Describe("limits", func() {
			It("caps groups and reports the omission", func() {
				for i := 0; i < 10; i++ {
					write(newDecision(withMetadata(map[string]string{
						"feature": string(rune('a' + i)),
					})))
				}

				r := report(ports.ReportSpec{
					GroupBy: ports.GroupByMetadata, MetaKey: "feature", Limit: 3,
				})

				Expect(r.Rows).To(HaveLen(3))
				Expect(r.TruncatedGroups).To(Equal(int64(7)),
					"a truncated report must say so rather than looking complete")
			})
		})

		Describe("scoping", func() {
			BeforeEach(func() {
				write(
					newDecision(withTenant("acme")),
					newDecision(withTenant("acme"), withTime(testTime.Add(-48*time.Hour))),
					newDecision(withTenant("globex")),
				)
			})

			It("scopes by tenant", func() {
				r := report(ports.ReportSpec{Tenant: "acme", Since: testTime.Add(-72 * time.Hour)})
				Expect(r.Total.Requests).To(Equal(int64(2)))
			})

			It("excludes rows at or after Until", func() {
				c, cancel := ctx()
				defer cancel()

				r, err := store.Aggregate(c, ports.ReportSpec{
					Tenant: "acme",
					Since:  testTime.Add(-72 * time.Hour),
					Until:  testTime,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(r.Total.Requests).To(Equal(int64(1)))
			})
		})
	})
})
