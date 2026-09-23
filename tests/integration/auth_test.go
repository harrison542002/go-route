//go:build integration

package integration

import (
	"crypto/sha256"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/ports"
)

var _ = Describe("Auth", func() {
	var auth *repositories.Auth

	BeforeEach(func() {
		truncate()
		auth = repositories.NewAuth(pool)
	})

	authenticate := func(key string) (ports.Identity, error) {
		c, cancel := ctx()
		defer cancel()
		return auth.Authenticate(c, key)
	}

	It("resolves the tenant from the key, never from the caller", func() {
		issueKey("acme", "gr_live_acme", nil)

		id, err := authenticate("gr_live_acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(id.Tenant).To(Equal(tenantName("acme")))
		Expect(id.KeyID).NotTo(Equal(uuid.Nil))
	})

	It("rejects a key it has never seen", func() {
		_, err := authenticate("gr_not_a_key")
		Expect(err).To(MatchError(ports.ErrUnknownKey))
	})

	It("rejects an empty credential without a query", func() {
		_, err := authenticate("")
		Expect(err).To(MatchError(ports.ErrNoCredentials))
	})

	// Only the hash is stored, so a database leak hands nobody a working
	// credential.
	It("stores no trace of the key itself", func() {
		issueKey("acme", "gr_live_secret", nil)

		Expect(countRows(
			"SELECT count(*) FROM api_keys WHERE encode(key_hash, 'escape') LIKE '%gr_live_secret%'")).
			To(BeZero())
		Expect(countRows("SELECT count(*) FROM api_keys WHERE key_hash = $1",
			sha256Of("gr_live_secret"))).To(Equal(1))
	})

	It("rejects a revoked key but keeps it attributable", func() {
		id := issueKey("acme", "gr_live_revoked", nil)

		c, cancel := ctx()
		defer cancel()
		_, err := pool.Exec(c, "UPDATE api_keys SET revoked_at = now() WHERE id = $1", id)
		Expect(err).NotTo(HaveOccurred())

		_, err = authenticate("gr_live_revoked")
		Expect(err).To(MatchError(ports.ErrKeyRevoked))

		Expect(countRows("SELECT count(*) FROM api_keys WHERE id = $1", id)).To(Equal(1),
			"revocation is a timestamp, not a delete")
	})

	It("rejects a live key belonging to a disabled tenant", func() {
		issueKey("globex", "gr_live_globex", nil)

		c, cancel := ctx()
		defer cancel()
		_, err := pool.Exec(c, "UPDATE tenants SET disabled_at = now() WHERE external_id = 'globex'")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			rc, rcancel := ctx()
			defer rcancel()
			_, _ = pool.Exec(rc, "UPDATE tenants SET disabled_at = NULL WHERE external_id = 'globex'")
		})

		_, err = authenticate("gr_live_globex")
		Expect(err).To(MatchError(ports.ErrTenantDisabled))
	})

	// Uncached on purpose: a revocation has to bite now, not whenever an
	// entry would have expired.
	It("sees a revocation without a restart", func() {
		id := issueKey("acme", "gr_live_rotating", nil)
		Expect(authenticate("gr_live_rotating")).Error().NotTo(HaveOccurred())

		c, cancel := ctx()
		defer cancel()
		_, err := pool.Exec(c, "UPDATE api_keys SET revoked_at = now() WHERE id = $1", id)
		Expect(err).NotTo(HaveOccurred())

		_, err = authenticate("gr_live_rotating")
		Expect(err).To(MatchError(ports.ErrKeyRevoked))
	})

	Describe("model allowlist", func() {
		// The distinction the column exists for. Getting these two round
		// the wrong way either bills a free plan for the expensive model
		// or locks every key out of everything.
		It("treats NULL as every alias", func() {
			issueKey("acme", "gr_live_null", nil)

			id, err := authenticate("gr_live_null")
			Expect(err).NotTo(HaveOccurred())
			Expect(id.Allowlist).To(BeNil())
			Expect(id.AllowsModel("anything")).To(BeTrue())
		})

		It("treats the empty array as no alias", func() {
			issueKey("acme", "gr_live_empty", []string{})

			id, err := authenticate("gr_live_empty")
			Expect(err).NotTo(HaveOccurred())
			Expect(id.Allowlist).NotTo(BeNil())
			Expect(id.AllowsModel("anything")).To(BeFalse())
		})

		It("admits only the aliases it lists", func() {
			issueKey("acme", "gr_live_tiered", []string{"cheap", "chat"})

			id, err := authenticate("gr_live_tiered")
			Expect(err).NotTo(HaveOccurred())
			Expect(id.AllowsModel("chat")).To(BeTrue())
			Expect(id.AllowsModel("expensive")).To(BeFalse())
		})
	})
})

func sha256Of(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// issueKey provisions an API key the way an admin API would.
func issueKey(tenant, key string, allowlist []string) uuid.UUID {
	c, cancel := ctx()
	defer cancel()

	id := uuid.New()
	_, err := pool.Exec(c, `
		INSERT INTO api_keys (id, tenant_id, key_hash, key_prefix, model_allowlist)
		VALUES ($1, (SELECT id FROM tenants WHERE external_id = $2), $3, $4, $5)`,
		id, tenant, sha256Of(key), key[:min(len(key), 10)], allowlist)
	Expect(err).NotTo(HaveOccurred())

	DeferCleanup(func() {
		rc, rcancel := ctx()
		defer rcancel()
		_, _ = pool.Exec(rc, "DELETE FROM api_keys WHERE id = $1", id)
	})
	return id
}
