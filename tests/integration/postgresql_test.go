//go:build integration

package integration

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/core/domains"
)

var _ = Describe("RecordWriter", func() {
	var writer *repositories.RecordWriter

	BeforeEach(func() {
		truncate()

		writer = repositories.NewRecordWriter(pool)
	})

	write := func(batch ...domains.RoutingDecision) error {
		c, cancel := ctx()
		defer cancel()
		return writer.Write(c, batch)
	}

	Describe("schema", func() {
		It("has created every table the design calls for", func() {
			Expect(countRows(`
				SELECT count(*) FROM information_schema.tables
				WHERE table_schema = 'public'
				  AND table_name IN ('tenants', 'api_keys', 'quotas', 'usage_counters',
				                     'usage_ledger', 'audit_log')`),
			).To(Equal(6))
		})

		It("has dropped the decisions table it replaced", func() {
			Expect(countRows(`
				SELECT count(*) FROM information_schema.tables
				WHERE table_name = 'decisions'`)).To(BeZero())
		})

		It("partitions the two record tables by time", func() {
			Expect(countRows(`
				SELECT count(*) FROM pg_partitioned_table p
				JOIN pg_class c ON c.oid = p.partrelid
				WHERE c.relname IN ('usage_ledger', 'audit_log')`)).To(Equal(2))
		})

		It("has partitions ahead of now", func() {
			Expect(countRows(`
				SELECT count(*) FROM pg_inherits i
				JOIN pg_class p ON p.oid = i.inhparent
				WHERE p.relname = 'usage_ledger'`)).To(BeNumerically(">=", 4),
				"the month before, the current one, and the months ahead")
		})

		It("creates no tenants of its own", func() {
			Expect(countRows("SELECT count(*) FROM tenants WHERE external_id = 'default'")).
				To(Equal(1), "provisioned by the suite, never by a migration or a write")
		})

		It("records what Atlas applied", func() {
			Expect(countRows(
				"SELECT count(*) FROM atlas_schema_revisions.atlas_schema_revisions")).
				To(BeNumerically(">=", 6))
		})

		It("documents the tables in the catalog", func() {
			Expect(countRows(`
				SELECT count(*) FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = 'public'
				  AND c.relname IN ('tenants', 'api_keys', 'quotas', 'usage_counters',
				                    'usage_ledger', 'audit_log')
				  AND obj_description(c.oid, 'pg_class') IS NOT NULL`)).To(Equal(6),
				"COMMENT ON puts the rationale where psql and tooling can see it")

			Expect(countRows(`
				SELECT count(*) FROM pg_attribute a
				WHERE a.attrelid = 'usage_ledger'::regclass
				  AND a.attnum > 0
				  AND col_description(a.attrelid, a.attnum) IS NOT NULL`)).
				To(BeNumerically(">=", 6))
		})

		It("creates the indexes reports depend on", func() {
			Expect(countRows(`
				SELECT count(*) FROM pg_indexes
				WHERE tablename = 'usage_ledger'
				  AND indexname IN ('usage_ledger_tenant_time', 'usage_ledger_metadata',
				                    'usage_ledger_target_time')`)).To(Equal(3))
		})
	})

	Describe("writing a batch", func() {
		It("persists a ledger row and an audit row per record", func() {
			Expect(write(newDecision(), newDecision(), newDecision())).To(Succeed())
			Expect(countRows("SELECT count(*) FROM usage_ledger")).To(Equal(3))
			Expect(countRows("SELECT count(*) FROM audit_log")).To(Equal(3))
		})

		It("gives both rows the same id, so explain is two point lookups", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())

			Expect(countRows(`
				SELECT count(*) FROM usage_ledger l
				JOIN audit_log a ON a.id = l.id
				WHERE l.id = $1`, d.ID.UUID())).To(Equal(1))
		})

		It("accepts an empty batch without touching the database", func() {
			Expect(write()).To(Succeed())
			Expect(countRows("SELECT count(*) FROM usage_ledger")).To(BeZero())
		})

		It("round-trips every ledger column", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())

			c, cancel := ctx()
			defer cancel()

			var (
				model, status, tokenSource                string
				in, out, cacheRead, cacheWrite, reasoning int
				chosen                                    *string
				costNanos                                 *int64
				pricingVersion                            *string
				ttft, total                               *int
				billable                                  bool
				startedAt                                 time.Time
			)
			err := pool.QueryRow(c, `
				SELECT requested_model, chosen_target, status, token_source,
				       input_tokens, output_tokens, cache_read_tokens,
				       cache_write_tokens, reasoning_tokens,
				       cost_nanos, pricing_version, billable,
				       ttft_ms, total_ms, started_at
				FROM usage_ledger WHERE id = $1`, d.ID.UUID()).
				Scan(&model, &chosen, &status, &tokenSource,
					&in, &out, &cacheRead, &cacheWrite, &reasoning,
					&costNanos, &pricingVersion, &billable,
					&ttft, &total, &startedAt)
			Expect(err).NotTo(HaveOccurred())

			Expect(model).To(Equal("chat"))
			Expect(chosen).NotTo(BeNil())
			Expect(*chosen).To(Equal("openai/gpt-5-mini"))
			Expect(status).To(Equal("ok"))
			Expect(tokenSource).To(Equal("provider"))

			Expect(in).To(Equal(80))
			Expect(out).To(Equal(50))
			Expect(cacheRead).To(Equal(20), "cached tokens must survive as their own column")
			Expect(cacheWrite).To(BeZero())
			Expect(reasoning).To(Equal(10))

			Expect(costNanos).NotTo(BeNil())
			Expect(*costNanos).To(Equal(int64(120_500)))
			Expect(pricingVersion).NotTo(BeNil())
			Expect(*pricingVersion).To(Equal("2026-08-01"))
			Expect(billable).To(BeTrue(), "a record is billed unless something says otherwise")

			Expect(ttft).NotTo(BeNil())
			Expect(*ttft).To(Equal(412))
			Expect(total).NotTo(BeNil())
			Expect(*total).To(Equal(1893))
			Expect(startedAt.UTC()).To(BeTemporally("==", testTime))
		})

		It("attributes the row to the tenant's id, not its name", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())

			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE tenant_id = $1",
				tenantID("default"))).To(Equal(1))
		})

		It("rejects a record whose tenant was never provisioned", func() {
			Expect(write(newDecision(withTenant("newco")))).NotTo(Succeed(),
				"the ledger must not invent the customer a charge is attributed to")

			Expect(countRows("SELECT count(*) FROM tenants WHERE external_id = 'newco'")).
				To(BeZero(), "a write must never create a tenant as a side effect")
		})

		It("attributes the row to the key that authorised it", func() {
			keyID := issueKey("acme", "gr_live_ledger", nil)
			Expect(write(newDecision(withTenant("acme"), withKey(keyID)))).To(Succeed())

			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE key_id = $1", keyID)).To(Equal(1))
			Expect(countRows("SELECT count(*) FROM audit_log WHERE key_id = $1", keyID)).To(Equal(1))
		})

		It("stores NULL when no key authorised the record", func() {
			Expect(write(newDecision())).To(Succeed())
			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE key_id IS NULL")).To(Equal(1),
				"a record with no credential behind it must not claim one")
		})

		It("round-trips the ledger's JSONB columns", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())

			c, cancel := ctx()
			defer cancel()

			var metadata, counterfactuals []byte
			Expect(pool.QueryRow(c, `
				SELECT metadata, counterfactuals
				FROM usage_ledger WHERE id = $1`, d.ID.UUID()).
				Scan(&metadata, &counterfactuals)).To(Succeed())

			var meta map[string]string
			Expect(json.Unmarshal(metadata, &meta)).To(Succeed())
			Expect(meta).To(HaveKeyWithValue("feature", "auto-tag"))

			var gotCounter []domains.Counterfactual
			Expect(json.Unmarshal(counterfactuals, &gotCounter)).To(Succeed())
			Expect(gotCounter).To(HaveLen(1))
			Expect(gotCounter[0].Target).To(Equal("openai/gpt-5"))
		})

		It("records the routing story in the audit row", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())

			c, cancel := ctx()
			defer cancel()

			var (
				actor, action            string
				reasonKind, reasonDetail *string
				finalTarget              *string
				ladder, attempts         []byte
			)
			Expect(pool.QueryRow(c, `
				SELECT actor, action, reason_kind, reason_detail, final_target,
				       ladder, ladder_attempts
				FROM audit_log WHERE id = $1`, d.ID.UUID()).
				Scan(&actor, &action, &reasonKind, &reasonDetail, &finalTarget,
					&ladder, &attempts)).To(Succeed())

			Expect(actor).To(Equal("gateway"))
			Expect(action).To(Equal("route_request"))
			Expect(reasonKind).NotTo(BeNil())
			Expect(*reasonKind).To(Equal("model_alias"))
			Expect(reasonDetail).NotTo(BeNil())
			Expect(*reasonDetail).To(Equal("chat"))
			Expect(finalTarget).NotTo(BeNil())
			Expect(*finalTarget).To(Equal("openai/gpt-5-mini"))

			var gotAttempts []domains.Attempt
			Expect(json.Unmarshal(attempts, &gotAttempts)).To(Succeed())
			Expect(gotAttempts).To(HaveLen(1))
			Expect(gotAttempts[0].Target).To(Equal("openai/gpt-5-mini"))

			var gotLadder []domains.TargetRef
			Expect(json.Unmarshal(ladder, &gotLadder)).To(Succeed())
			Expect(gotLadder).To(HaveLen(1))
		})
	})

	Describe("unpriced records", func() {
		BeforeEach(func() {
			Expect(write(
				newDecision(),
				newDecision(unpriced()),
				newDecision(unpriced()),
			)).To(Succeed())
		})

		It("stores NULL rather than zero", func() {
			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE cost_nanos IS NULL")).To(Equal(2))
			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE cost_nanos = 0")).To(BeZero(),
				"an unpriced record stored as zero would be indistinguishable from a free one")
		})

		It("is excluded from SUM but countable", func() {
			c, cancel := ctx()
			defer cancel()

			var total int64
			var unpricedCount int
			Expect(pool.QueryRow(c, `
				SELECT COALESCE(SUM(cost_nanos), 0),
				       count(*) FILTER (WHERE cost_nanos IS NULL)
				FROM usage_ledger`).Scan(&total, &unpricedCount)).To(Succeed())

			Expect(total).To(Equal(int64(120_500)), "only the priced row contributes")
			Expect(unpricedCount).To(Equal(2), "a report must be able to disclose its coverage")
		})
	})

	Describe("exhausted records", func() {
		It("stores a NULL chosen_target", func() {
			Expect(write(newDecision(exhausted()))).To(Succeed())

			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE chosen_target IS NULL")).To(Equal(1))
			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE status = 'exhausted'")).To(Equal(1))
		})

		It("preserves the failure detail in the audit row", func() {
			d := newDecision(exhausted())
			Expect(write(d)).To(Succeed())

			c, cancel := ctx()
			defer cancel()

			var attempts []byte
			var errMessage *string
			Expect(pool.QueryRow(c,
				"SELECT ladder_attempts, error FROM audit_log WHERE id = $1", d.ID.UUID()).
				Scan(&attempts, &errMessage)).To(Succeed())

			var got []domains.Attempt
			Expect(json.Unmarshal(attempts, &got)).To(Succeed())
			Expect(got[0].Failure).NotTo(BeNil())
			Expect(got[0].Failure.Kind).To(Equal("connect"))

			Expect(errMessage).NotTo(BeNil())
			Expect(*errMessage).To(Equal("connection refused"),
				"the last failure belongs in a column, not only inside the JSON")
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
				SELECT count(*) FROM usage_ledger
				WHERE metadata @> '{"feature":"auto-tag"}'`)).To(Equal(2))
		})

		It("groups spend by a metadata key", func() {
			c, cancel := ctx()
			defer cancel()

			rows, err := pool.Query(c, `
				SELECT metadata->>'feature', count(*), SUM(cost_nanos)
				FROM usage_ledger
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
			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE metadata = '{}'")).To(Equal(1))
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
			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE tenant_id = $1",
				tenantID("acme"))).To(Equal(2))
		})

		It("scopes by time range within a tenant", func() {
			Expect(countRows(`
				SELECT count(*) FROM usage_ledger
				WHERE tenant_id = $1 AND started_at >= $2`,
				tenantID("acme"), testTime.Add(-24*time.Hour))).To(Equal(1))
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

			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE id = $1", good.ID.UUID())).
				To(BeZero(), "COPY is all-or-nothing; a partial batch would be harder to reason about")
		})

		It("leaves no ledger row behind when the audit write fails", func() {
			d := newDecision()
			Expect(write(d)).To(Succeed())

			c, cancel := ctx()
			defer cancel()
			_, err := pool.Exec(c, "DELETE FROM usage_ledger WHERE id = $1", d.ID.UUID())
			Expect(err).NotTo(HaveOccurred())

			// The surviving audit row makes the replay fail on audit_log's
			// primary key rather than on the ledger's.
			Expect(write(d)).NotTo(Succeed())
			Expect(countRows("SELECT count(*) FROM usage_ledger WHERE id = $1", d.ID.UUID())).
				To(BeZero(), "the ledger write must roll back with the audit write")
		})

		It("rejects a record outside every partition", func() {
			far := newDecision(withTime(testTime.AddDate(5, 0, 0)))
			Expect(write(far)).NotTo(Succeed(),
				"a record with nowhere to go must fail loudly rather than vanish")
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

		rows, err := pool.Query(c, "SELECT id FROM usage_ledger ORDER BY id")
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
