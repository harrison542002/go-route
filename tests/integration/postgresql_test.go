//go:build integration

package integration

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/core/domains"
)

var _ = Describe("PostgresWriter", func() {
	var writer *sink.PostgresWriter

	BeforeEach(func() {
		truncate()

		c, cancel := ctx()
		defer cancel()

		var err error
		writer, err = sink.NewPostgresWriter(c, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(writer.Close)
	})

	write := func(batch ...domains.RoutingDecision) error {
		c, cancel := ctx()
		defer cancel()
		return writer.Write(c, batch)
	}

	Describe("schema", func() {
		It("has created the decisions table", func() {
			Expect(countRows(`
				SELECT count(*) FROM information_schema.tables
				WHERE table_name = 'decisions'`)).To(Equal(1))
		})

		It("is idempotent across restarts", func() {
			c, cancel := ctx()
			defer cancel()

			second, err := sink.NewPostgresWriter(c, dsn)
			Expect(err).NotTo(HaveOccurred(), "a restart must not fail on an existing schema")
			Expect(second.Close()).To(Succeed())
		})

		It("creates the indexes reports depend on", func() {
			Expect(countRows(`
				SELECT count(*) FROM pg_indexes
				WHERE tablename = 'decisions'
				  AND indexname IN ('decisions_tenant_time', 'decisions_metadata', 'decisions_target_time')`),
			).To(Equal(3))
		})
	})

	Describe("writing a batch", func() {
		It("persists every row", func() {
			Expect(write(newDecision(), newDecision(), newDecision())).To(Succeed())
			Expect(countRows("SELECT count(*) FROM decisions")).To(Equal(3))
		})

		It("accepts an empty batch without touching the database", func() {
			Expect(write()).To(Succeed())
			Expect(countRows("SELECT count(*) FROM decisions")).To(BeZero())
		})

		It("round-trips every scalar column", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())

			c, cancel := ctx()
			defer cancel()

			var (
				tenant, model, chosen, status, reasonKind, reasonDetail string
				in, out, cacheRead, reasoning, attemptCount             int
				costNanos                                               *int64
				priceTable                                              *string
				ttft, total                                             int
				occurredAt                                              time.Time
			)
			err := pool.QueryRow(c, `
				SELECT tenant, requested_model, chosen_target, status,
				       reason_kind, reason_detail,
				       input_tokens, output_tokens, cache_read_tokens, reasoning_tokens,
				       attempt_count, cost_nanos, price_table_version,
				       ttft_ms, total_ms, occurred_at
				FROM decisions WHERE id = $1`, d.ID.UUID()).
				Scan(&tenant, &model, &chosen, &status, &reasonKind, &reasonDetail,
					&in, &out, &cacheRead, &reasoning, &attemptCount,
					&costNanos, &priceTable, &ttft, &total, &occurredAt)
			Expect(err).NotTo(HaveOccurred())

			Expect(tenant).To(Equal("default"))
			Expect(model).To(Equal("chat"))
			Expect(chosen).To(Equal("openai/gpt-5-mini"))
			Expect(status).To(Equal("ok"))
			Expect(reasonKind).To(Equal("model_alias"))
			Expect(reasonDetail).To(Equal("chat"))

			Expect(in).To(Equal(80))
			Expect(out).To(Equal(50))
			Expect(cacheRead).To(Equal(20), "cached tokens must survive as their own column")
			Expect(reasoning).To(Equal(10))
			Expect(attemptCount).To(Equal(1))

			Expect(costNanos).NotTo(BeNil())
			Expect(*costNanos).To(Equal(int64(120_500)))
			Expect(priceTable).NotTo(BeNil())
			Expect(*priceTable).To(Equal("2026-08-01"))

			Expect(ttft).To(Equal(412))
			Expect(total).To(Equal(1893))
			Expect(occurredAt.UTC()).To(BeTemporally("==", testTime))
		})

		It("round-trips the JSONB columns", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())

			c, cancel := ctx()
			defer cancel()

			var metadata, attempts, counterfactuals, ladder []byte
			Expect(pool.QueryRow(c, `
				SELECT metadata, attempts, counterfactuals, ladder
				FROM decisions WHERE id = $1`, d.ID.UUID()).
				Scan(&metadata, &attempts, &counterfactuals, &ladder)).To(Succeed())

			var meta map[string]string
			Expect(json.Unmarshal(metadata, &meta)).To(Succeed())
			Expect(meta).To(HaveKeyWithValue("feature", "auto-tag"))

			var gotAttempts []domains.Attempt
			Expect(json.Unmarshal(attempts, &gotAttempts)).To(Succeed())
			Expect(gotAttempts).To(HaveLen(1))
			Expect(gotAttempts[0].Target).To(Equal("openai/gpt-5-mini"))

			var gotCounter []domains.Counterfactual
			Expect(json.Unmarshal(counterfactuals, &gotCounter)).To(Succeed())
			Expect(gotCounter).To(HaveLen(1))
			Expect(gotCounter[0].Target).To(Equal("openai/gpt-5"))

			var gotLadder []domains.TargetRef
			Expect(json.Unmarshal(ladder, &gotLadder)).To(Succeed())
			Expect(gotLadder).To(HaveLen(1))
		})
	})

	Describe("unpriced decisions", func() {
		BeforeEach(func() {
			Expect(write(
				newDecision(),
				newDecision(unpriced()),
				newDecision(unpriced()),
			)).To(Succeed())
		})

		It("stores NULL rather than zero", func() {
			Expect(countRows("SELECT count(*) FROM decisions WHERE cost_nanos IS NULL")).To(Equal(2))
			Expect(countRows("SELECT count(*) FROM decisions WHERE cost_nanos = 0")).To(BeZero(),
				"an unpriced decision stored as zero would be indistinguishable from a free one")
		})

		It("is excluded from SUM but countable", func() {
			c, cancel := ctx()
			defer cancel()

			var total int64
			var unpricedCount int
			Expect(pool.QueryRow(c, `
				SELECT COALESCE(SUM(cost_nanos), 0),
				       count(*) FILTER (WHERE cost_nanos IS NULL)
				FROM decisions`).Scan(&total, &unpricedCount)).To(Succeed())

			Expect(total).To(Equal(int64(120_500)), "only the priced row contributes")
			Expect(unpricedCount).To(Equal(2), "a report must be able to disclose its coverage")
		})
	})

	Describe("exhausted decisions", func() {
		It("stores a NULL chosen_target", func() {
			Expect(write(newDecision(exhausted()))).To(Succeed())

			Expect(countRows("SELECT count(*) FROM decisions WHERE chosen_target IS NULL")).To(Equal(1))
			Expect(countRows("SELECT count(*) FROM decisions WHERE status = 'exhausted'")).To(Equal(1))
		})

		It("preserves the failure detail in attempts", func() {
			d := newDecision(exhausted())
			Expect(write(d)).To(Succeed())

			c, cancel := ctx()
			defer cancel()

			var attempts []byte
			Expect(pool.QueryRow(c, "SELECT attempts FROM decisions WHERE id = $1", d.ID.UUID()).
				Scan(&attempts)).To(Succeed())

			var got []domains.Attempt
			Expect(json.Unmarshal(attempts, &got)).To(Succeed())
			Expect(got[0].Failure).NotTo(BeNil())
			Expect(got[0].Failure.Kind).To(Equal("connect"))
		})
	})

	Describe("metadata queries", func() {
		BeforeEach(func() {
			Expect(write(
				newDecision(withMetadata(map[string]string{"feature": "auto-tag"})),
				newDecision(withMetadata(map[string]string{"feature": "auto-tag"})),
				newDecision(withMetadata(map[string]string{"feature": "chat-widget"})),
				newDecision(withMetadata(map[string]string{})),
			)).To(Succeed())
		})

		It("finds rows by containment", func() {
			Expect(countRows(`
				SELECT count(*) FROM decisions
				WHERE metadata @> '{"feature":"auto-tag"}'`)).To(Equal(2))
		})

		It("groups spend by a metadata key", func() {
			c, cancel := ctx()
			defer cancel()

			rows, err := pool.Query(c, `
				SELECT metadata->>'feature', count(*), SUM(cost_nanos)
				FROM decisions
				WHERE metadata ? 'feature'
				GROUP BY 1 ORDER BY 1`)
			Expect(err).NotTo(HaveOccurred())
			defer rows.Close()

			byFeature := map[string]int{}
			for rows.Next() {
				var feature string
				var count int
				var total int64
				Expect(rows.Scan(&feature, &count, &total)).To(Succeed())
				byFeature[feature] = count
			}
			Expect(rows.Err()).NotTo(HaveOccurred())

			Expect(byFeature).To(HaveKeyWithValue("auto-tag", 2))
			Expect(byFeature).To(HaveKeyWithValue("chat-widget", 1))
		})

		It("stores an empty map as {} rather than null", func() {
			Expect(countRows("SELECT count(*) FROM decisions WHERE metadata = '{}'")).To(Equal(1))
		})
	})

	Describe("tenant and time filtering", func() {
		BeforeEach(func() {
			Expect(write(
				newDecision(withTenant("acme"), withTime(testTime)),
				newDecision(withTenant("acme"), withTime(testTime.Add(-48*time.Hour))),
				newDecision(withTenant("globex"), withTime(testTime)),
			)).To(Succeed())
		})

		It("scopes by tenant", func() {
			Expect(countRows("SELECT count(*) FROM decisions WHERE tenant = $1", "acme")).To(Equal(2))
		})

		It("scopes by time range within a tenant", func() {
			Expect(countRows(`
				SELECT count(*) FROM decisions
				WHERE tenant = $1 AND occurred_at >= $2`,
				"acme", testTime.Add(-24*time.Hour))).To(Equal(1))
		})
	})

	Describe("failure modes", func() {
		It("rejects a duplicate ID", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())
			Expect(write(d)).NotTo(Succeed(), "the primary key must reject a replayed record")
		})

		It("fails the whole batch when one row is bad", func() {
			good := newDecision()
			dup := newDecision()

			Expect(write(dup)).To(Succeed())
			Expect(write(good, dup)).NotTo(Succeed())

			Expect(countRows("SELECT count(*) FROM decisions WHERE id = $1", good.ID.UUID())).
				To(BeZero(), "COPY is all-or-nothing; a partial batch would be harder to reason about")
		})

		It("errors on a cancelled context rather than hanging", func() {
			c, cancel := ctx()
			cancel()

			Expect(writer.Write(c, []domains.RoutingDecision{newDecision()})).NotTo(Succeed())
		})
	})

	It("stores IDs that sort in insertion order", func() {
		var ids []domains.DecisionID
		for i := 0; i < 20; i++ {
			d := newDecision()
			ids = append(ids, d.ID)
			Expect(write(d)).To(Succeed())
		}

		c, cancel := ctx()
		defer cancel()

		rows, err := pool.Query(c, "SELECT id FROM decisions ORDER BY id")
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()

		var i int
		for rows.Next() {
			var got [16]byte
			Expect(rows.Scan(&got)).To(Succeed())
			Expect(got).To(Equal([16]byte(ids[i].UUID())), "row %d is out of insertion order", i)
			i++
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(i).To(Equal(20))
	})
})
