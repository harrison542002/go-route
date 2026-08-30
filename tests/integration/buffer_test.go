//go:build integration

package integration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
)

// The buffered sink and the Postgres writer have only ever been tested
// apart. Batching, the flush interval, and shutdown draining are where
// they interact.
var _ = Describe("Buffered sink over Postgres", func() {
	var (
		writer *sink.PostgresWriter
		s      *sink.Buffered
	)

	newSink := func(cfg sink.Config) {
		c, cancel := ctx()
		defer cancel()

		var err error
		writer, err = sink.NewPostgresWriter(c, dsn)
		Expect(err).NotTo(HaveOccurred())

		s = sink.NewBuffered(writer, cfg)

		DeferCleanup(func() {
			fc, fcancel := ctx()
			defer fcancel()
			_ = s.Flush(fc)
			_ = writer.Close()
		})
	}

	BeforeEach(truncate)

	It("writes when the batch fills", func() {
		newSink(sink.Config{BatchSize: 5, FlushInterval: time.Hour})

		for i := 0; i < 5; i++ {
			s.Record(newDecision())
		}

		Eventually(func() int {
			return countRows("SELECT count(*) FROM decisions")
		}, "5s", "50ms").Should(Equal(5))
	})

	It("writes a partial batch on the flush interval", func() {
		newSink(sink.Config{BatchSize: 100, FlushInterval: 100 * time.Millisecond})

		s.Record(newDecision())
		s.Record(newDecision())

		Eventually(func() int {
			return countRows("SELECT count(*) FROM decisions")
		}, "5s", "50ms").Should(Equal(2),
			"a low-traffic deployment must not sit on records indefinitely")
	})

	It("drains everything buffered on Flush", func() {
		newSink(sink.Config{BatchSize: 1000, FlushInterval: time.Hour})

		for i := 0; i < 250; i++ {
			s.Record(newDecision())
		}

		c, cancel := ctx()
		defer cancel()
		Expect(s.Flush(c)).To(Succeed())

		Expect(countRows("SELECT count(*) FROM decisions")).To(Equal(250),
			"closing the channel must drain the buffer, not discard it")
	})

	It("is safe to flush twice", func() {
		newSink(sink.Config{})
		s.Record(newDecision())

		c, cancel := ctx()
		defer cancel()
		Expect(s.Flush(c)).To(Succeed())
		Expect(s.Flush(c)).To(Succeed())
	})

	It("drops rather than blocking when the buffer is full", func() {
		newSink(sink.Config{BufferSize: 4, BatchSize: 1000, FlushInterval: time.Hour})

		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 2000; i++ {
				s.Record(newDecision())
			}
		}()

		Eventually(done, "5s").Should(BeClosed(),
			"Record must never block: a slow writer would otherwise make every request slow")

		_, dropped, _ := s.Stats()
		Expect(dropped).To(BeNumerically(">", 0), "drops must be counted, not silent")
	})

	It("records survive a full round trip with their cost intact", func() {
		newSink(sink.Config{BatchSize: 1, FlushInterval: time.Hour})

		d := newDecision()
		s.Record(d)

		Eventually(func() int {
			return countRows("SELECT count(*) FROM decisions WHERE id = $1", d.ID.UUID())
		}, "5s", "50ms").Should(Equal(1))

		c, cancel := ctx()
		defer cancel()

		var costNanos *int64
		Expect(pool.QueryRow(c, "SELECT cost_nanos FROM decisions WHERE id = $1", d.ID.UUID()).
			Scan(&costNanos)).To(Succeed())
		Expect(costNanos).NotTo(BeNil())
		Expect(*costNanos).To(Equal(int64(120_500)))
	})
})
