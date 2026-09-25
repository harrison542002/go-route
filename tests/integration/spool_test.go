//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/core/domains"
)

// The spool and the Postgres writer meet here: shipping on the flush
// interval, draining on shutdown, surviving an outage, and replaying
// without duplicating rows.
var _ = Describe("Spooled sink over Postgres", Ordered, func() {
	// outagePool connects as a role the specs can lock out, so the
	// database goes away for the sink without going away for the suite.
	var outagePool *pgxpool.Pool

	BeforeAll(func() {
		c, cancel := ctx()
		defer cancel()

		_, err := pool.Exec(c, `
			DO $$ BEGIN
				CREATE ROLE spool_writer LOGIN PASSWORD 'spool';
			EXCEPTION WHEN duplicate_object THEN NULL;
			END $$`)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(c, `GRANT SELECT ON tenants TO spool_writer`)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(c, `GRANT SELECT, INSERT ON usage_ledger, audit_log TO spool_writer`)
		Expect(err).NotTo(HaveOccurred())

		cfg, err := pgxpool.ParseConfig(dsn)
		Expect(err).NotTo(HaveOccurred())
		cfg.ConnConfig.User = "spool_writer"
		cfg.ConnConfig.Password = "spool"
		outagePool, err = pgxpool.NewWithConfig(c, cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(outagePool.Close)
	})

	databaseDown := func(down bool) {
		c, cancel := ctx()
		defer cancel()

		login := "LOGIN"
		if down {
			login = "NOLOGIN"
		}
		_, err := pool.Exec(c, "ALTER ROLE spool_writer "+login)
		Expect(err).NotTo(HaveOccurred())

		if down {
			// Connections already open would otherwise carry on working.
			_, err = pool.Exec(c, `
				SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE usename = 'spool_writer'`)
			Expect(err).NotTo(HaveOccurred())
		}
	}

	var dir string

	open := func(cfg sink.Config) *sink.Spooled {
		cfg.Dir = dir
		if cfg.FlushInterval == 0 {
			cfg.FlushInterval = 50 * time.Millisecond
		}
		cfg.RetryBackoff = 10 * time.Millisecond
		cfg.MaxRetryBackoff = 50 * time.Millisecond

		s, err := sink.NewSpooled(repositories.NewRecordWriter(outagePool), cfg, time.Now)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			fc, fcancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer fcancel()
			_ = s.Flush(fc)
		})
		return s
	}

	flush := func(s *sink.Spooled, d time.Duration) error {
		fc, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return s.Flush(fc)
	}

	segments := func() []string {
		names, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		Expect(err).NotTo(HaveOccurred())
		return names
	}

	ledgerRows := func() int { return countRows("SELECT count(*) FROM usage_ledger") }

	BeforeEach(func() {
		truncate()
		databaseDown(false)
		dir = GinkgoT().TempDir()
	})

	It("ships a partial segment on the flush interval", func() {
		s := open(sink.Config{BatchSize: 100})

		s.Record(newDecision())
		s.Record(newDecision())

		Eventually(ledgerRows, "5s", "20ms").Should(Equal(2),
			"a low-traffic deployment must not sit on records indefinitely")
		Eventually(segments, "5s", "20ms").Should(BeEmpty(),
			"a segment is deleted once its rows are committed")
	})

	It("ships everything on Flush", func() {
		s := open(sink.Config{BatchSize: 1000, FlushInterval: time.Hour})

		for range 250 {
			s.Record(newDecision())
		}
		Expect(flush(s, 10*time.Second)).To(Succeed())

		Expect(ledgerRows()).To(Equal(250))
		Expect(countRows("SELECT count(*) FROM audit_log")).To(Equal(250))
	})

	It("is safe to flush twice", func() {
		s := open(sink.Config{})
		s.Record(newDecision())

		Expect(flush(s, 5*time.Second)).To(Succeed())
		Expect(flush(s, 5*time.Second)).To(Succeed())
	})

	It("keeps every record through a database outage, without blocking", func() {
		s := open(sink.Config{BatchSize: 50})
		databaseDown(true)

		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 500 {
				s.Record(newDecision())
			}
		}()
		Eventually(done, "5s").Should(BeClosed(),
			"Record must not wait on the database: an outage must not make every request slow")

		Eventually(func() int64 { return s.Stats().Spooled }, "5s", "20ms").Should(Equal(int64(500)))
		Consistently(ledgerRows, "300ms", "50ms").Should(BeZero())
		Expect(s.Stats().Failures).To(BeNumerically(">", 0), "failed writes are counted and retried")

		databaseDown(false)

		Eventually(ledgerRows, "20s", "50ms").Should(Equal(500),
			"a recovered database receives everything the outage held back")
		Expect(countRows("SELECT count(*) FROM audit_log")).To(Equal(500))
		Eventually(segments, "5s", "20ms").Should(BeEmpty())
	})

	It("keeps records across a restart when the database never came back", func() {
		databaseDown(true)
		first := open(sink.Config{})

		for range 50 {
			first.Record(newDecision())
		}
		Expect(flush(first, 500*time.Millisecond)).To(Succeed(),
			"records on disk are not lost, so shutdown must not report them as lost")
		Expect(segments()).NotTo(BeEmpty())
		Expect(ledgerRows()).To(BeZero())

		databaseDown(false)
		open(sink.Config{})

		Eventually(ledgerRows, "10s", "50ms").Should(Equal(50))
	})

	It("does not duplicate rows when a replay re-sends committed records", func() {
		databaseDown(true)
		first := open(sink.Config{})

		var batch []domains.RoutingDecision
		for range 30 {
			d := newDecision()
			batch = append(batch, d)
			first.Record(d)
		}
		Expect(flush(first, 500*time.Millisecond)).To(Succeed())
		databaseDown(false)

		// These rows commit, but the segment holding them survives -- the
		// window between a commit and the delete that a crash can hit.
		c, cancel := ctx()
		defer cancel()
		Expect(repositories.NewRecordWriter(pool).Write(c, batch[:20])).To(Succeed())

		second := open(sink.Config{})
		Eventually(segments, "10s", "50ms").Should(BeEmpty())

		Expect(ledgerRows()).To(Equal(30), "exactly one row per decision")
		Expect(countRows("SELECT count(*) FROM audit_log")).To(Equal(30))
		Expect(second.Stats().DeadLettered).To(BeZero(),
			"a duplicate is a no-op, not a poison record")
	})

	It("dead-letters a record the database will never accept, and ships the rest", func() {
		s := open(sink.Config{BatchSize: 100, FlushInterval: time.Hour})

		for i := range 10 {
			if i == 4 {
				s.Record(newDecision(withTenant("never-provisioned")))
				continue
			}
			s.Record(newDecision())
		}
		Expect(flush(s, 10*time.Second)).To(Succeed())

		Expect(ledgerRows()).To(Equal(9), "one poison record must not take its batch down")
		Expect(countRows("SELECT count(*) FROM usage_ledger WHERE tenant_id = $1",
			tenantID("default"))).To(Equal(9))
		Expect(s.Stats().DeadLettered).To(Equal(int64(1)))
		Expect(segments()).To(BeEmpty(), "a poison record must not wedge the spool")

		dead, err := os.ReadDir(filepath.Join(dir, "dead"))
		Expect(err).NotTo(HaveOccurred())
		Expect(dead).To(HaveLen(1), "the rejected record is kept for an operator, not deleted")
	})

	It("records survive a full round trip with their cost intact", func() {
		s := open(sink.Config{})

		d := newDecision()
		s.Record(d)

		Eventually(func() int {
			return countRows("SELECT count(*) FROM usage_ledger WHERE id = $1", d.ID.UUID())
		}, "5s", "20ms").Should(Equal(1))

		c, cancel := ctx()
		defer cancel()

		var costNanos *int64
		Expect(pool.QueryRow(c, "SELECT cost_nanos FROM usage_ledger WHERE id = $1", d.ID.UUID()).
			Scan(&costNanos)).To(Succeed())
		Expect(costNanos).NotTo(BeNil())
		Expect(*costNanos).To(Equal(int64(120_500)))
	})
})
