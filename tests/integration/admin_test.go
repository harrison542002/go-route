//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/bootstrap"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/admin"
)

type adminResponse struct {
	status int
	header http.Header
	body   string
}

func (r adminResponse) decode(v any) {
	ExpectWithOffset(1, json.Unmarshal([]byte(r.body), v)).To(Succeed(), r.body)
}

// builtAdmin is the admin service as bootstrap composes it for the real binary,
// built once; every spec truncates the tables it cares about.
var (
	buildAdminOnce sync.Once
	adminApp       *bootstrap.AdminApp
)

func builtAdmin() *bootstrap.AdminApp {
	buildAdminOnce.Do(func() {
		c, cancel := ctx()
		defer cancel()

		app, err := bootstrap.BuildAdmin(c, bootstrap.AdminOptions{
			DSN:            dsn,
			Listen:         "127.0.0.1:0",
			RequestTimeout: bootstrap.DefaultAdminRequestTimeout,
			JWTSecret:      []byte(adminSigningSecret),
			AccessTokenTTL: bootstrap.DefaultAccessTokenTTL,
		})
		Expect(err).NotTo(HaveOccurred())
		adminApp = app
	})
	return adminApp
}

var _ = AfterSuite(func() {
	if adminApp != nil {
		Expect(adminApp.Close()).To(Succeed())
	}
})

var _ = Describe("Admin API", func() {
	var (
		srv        *httptest.Server
		creds      *admin.Credentials
		credential ports.IssuedAdminCredential
		adminToken string
		actorName  string
		external   string
	)

	BeforeEach(func() {
		truncate()

		store := repositories.NewAdminCredentialRepo(pool)
		creds = admin.NewCredentials(store, time.Now, nil)

		c, cancel := ctx()
		defer cancel()
		actorName = "integration-" + uuid.NewString()
		var err error
		credential, err = creds.Create(c, ports.Write{Actor: "cli:test"},
			ports.NewAdminCredential{Name: actorName, Role: domains.RoleAdmin})
		Expect(err).NotTo(HaveOccurred())
		adminToken = credential.Secret

		srv = httptest.NewServer(builtAdmin().Server.Handler)
		DeferCleanup(srv.Close)

		external = "adm-" + uuid.NewString()
	})

	call := func(method, path, body string, header ...string) adminResponse {
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+adminToken)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)

		return adminResponse{status: resp.StatusCode, header: resp.Header, body: string(raw)}
	}

	createTenant := func() string {
		r := call("POST", "/admin/v1/tenants", `{"external_id":"`+external+`","name":"Acme","metadata":{"plan":"pro"}}`)
		Expect(r.status).To(Equal(http.StatusCreated), r.body)
		var t struct{ ID string }
		r.decode(&t)
		return t.ID
	}

	auditRows := func(action string) int {
		return countRows(`SELECT count(*) FROM audit_log a JOIN tenants t ON t.id = a.tenant_id
			WHERE t.external_id = $1 AND a.action = $2 AND a.actor = $3`,
			external, action, "admin:"+actorName)
	}

	Describe("tenants", func() {
		It("returns the original tenant to a retried create and conflicts on a different one", func() {
			id := createTenant()

			again := call("POST", "/admin/v1/tenants", `{ "metadata": {"plan": "pro"}, "name": "Acme", "external_id": "`+external+`" }`)
			Expect(again.status).To(Equal(http.StatusOK), again.body)
			Expect(again.body).To(ContainSubstring(id))

			other := call("POST", "/admin/v1/tenants", `{"external_id":"`+external+`","name":"Globex"}`)
			Expect(other.status).To(Equal(http.StatusConflict), other.body)

			Expect(countRows("SELECT count(*) FROM tenants WHERE external_id = $1", external)).To(Equal(1))
			Expect(auditRows("tenant.create")).To(Equal(1))
		})

		It("disables instead of deleting, and a second delete changes nothing", func() {
			id := createTenant()

			first := call("DELETE", "/admin/v1/tenants/"+external, "")
			Expect(first.status).To(Equal(http.StatusOK), first.body)
			Expect(first.body).NotTo(ContainSubstring(`"disabled_at":null`))

			second := call("DELETE", "/admin/v1/tenants/"+id, "")
			Expect(second.status).To(Equal(http.StatusOK), second.body)

			Expect(countRows("SELECT count(*) FROM tenants WHERE external_id = $1 AND disabled_at IS NOT NULL", external)).To(Equal(1))
			Expect(auditRows("tenant.disable")).To(Equal(1), "the no-op must not be audited")

			Expect(call("POST", "/admin/v1/tenants/"+external+"/enable", "").status).To(Equal(http.StatusOK))
			Expect(countRows("SELECT count(*) FROM tenants WHERE external_id = $1 AND disabled_at IS NULL", external)).To(Equal(1))
		})

		It("patches name and metadata but never the external id", func() {
			createTenant()

			r := call("PATCH", "/admin/v1/tenants/"+external, `{"name":"Acme Corp","metadata":{"plan":"enterprise"}}`)
			Expect(r.status).To(Equal(http.StatusOK), r.body)
			Expect(r.body).To(ContainSubstring(`"plan":"enterprise"`))

			bad := call("PATCH", "/admin/v1/tenants/"+external, `{"external_id":"renamed"}`)
			Expect(bad.status).To(Equal(http.StatusBadRequest))
		})

		It("is 404 for a tenant that does not exist", func() {
			Expect(call("GET", "/admin/v1/tenants/"+external, "").status).To(Equal(http.StatusNotFound))
			Expect(call("GET", "/admin/v1/tenants/"+uuid.NewString(), "").status).To(Equal(http.StatusNotFound))
		})
	})

	Describe("repeating a write", func() {
		It("leaves a patched tenant where the last writer put it", func() {
			createTenant()

			Expect(call("PATCH", "/admin/v1/tenants/"+external, `{"name":"Renamed"}`).status).To(Equal(http.StatusOK))
			again := call("PATCH", "/admin/v1/tenants/"+external, `{"name":"Renamed"}`)
			Expect(again.status).To(Equal(http.StatusOK), again.body)
			Expect(again.body).To(ContainSubstring(`"Renamed"`))

			Expect(countRows("SELECT count(*) FROM tenants WHERE external_id = $1 AND name = 'Renamed'", external)).To(Equal(1))
			Expect(auditRows("tenant.update")).To(Equal(1), "the second patch changed nothing, so it is not audited")
		})

		It("puts the same quota twice without rewriting it", func() {
			createTenant()
			path := "/admin/v1/tenants/" + external + "/quotas/day"

			first := call("PUT", path, `{"max_requests":500}`)
			Expect(first.status).To(Equal(http.StatusOK), first.body)
			again := call("PUT", path, `{"max_requests":500}`)
			Expect(again.status).To(Equal(http.StatusOK), again.body)
			Expect(again.body).To(ContainSubstring(`"max_requests":500`))

			Expect(countRows(`SELECT count(*) FROM quotas q JOIN tenants t ON t.id = q.tenant_id
				WHERE t.external_id = $1 AND q.window_kind = 'day'`, external)).To(Equal(1))
			Expect(auditRows("quota.upsert")).To(Equal(1), "an unchanged limit is not a change")
		})

		It("deletes a quota that is already gone", func() {
			createTenant()
			path := "/admin/v1/tenants/" + external + "/quotas/hour"
			Expect(call("PUT", path, `{"max_requests":10}`).status).To(Equal(http.StatusOK))

			Expect(call("DELETE", path, "").status).To(Equal(http.StatusNoContent))
			Expect(call("DELETE", path, "").status).To(Equal(http.StatusNoContent))
			Expect(auditRows("quota.delete")).To(Equal(1))
		})
	})

	Describe("api keys", func() {
		keysPath := func() string { return "/admin/v1/tenants/" + external + "/keys" }

		It("creates, authenticates and revokes a key", func() {
			createTenant()
			auth := repositories.NewAuth(pool)

			created := call("POST", keysPath(), `{"model_allowlist":["chat"]}`)
			Expect(created.status).To(Equal(http.StatusCreated), created.body)

			var issued struct {
				ID        string
				KeyPrefix string `json:"key_prefix"`
				Secret    string
			}
			created.decode(&issued)
			Expect(issued.Secret).To(HavePrefix(admin.KeyPrefix))
			Expect(issued.Secret).To(HavePrefix(issued.KeyPrefix))

			c, cancel := ctx()
			defer cancel()
			id, err := auth.Authenticate(c, issued.Secret)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(id.Tenant)).To(Equal(external))
			Expect(id.AllowsModel("chat")).To(BeTrue())
			Expect(id.AllowsModel("expensive")).To(BeFalse())

			Expect(call("PATCH", "/admin/v1/keys/"+issued.ID, `{"model_allowlist":[]}`).status).To(Equal(http.StatusOK))
			id, err = auth.Authenticate(c, issued.Secret)
			Expect(err).NotTo(HaveOccurred())
			Expect(id.AllowsModel("chat")).To(BeFalse())

			Expect(call("DELETE", "/admin/v1/keys/"+issued.ID, "").status).To(Equal(http.StatusOK))
			_, err = auth.Authenticate(c, issued.Secret)
			Expect(err).To(MatchError(ports.ErrKeyRevoked))

			again := call("DELETE", "/admin/v1/keys/"+issued.ID, "")
			Expect(again.status).To(Equal(http.StatusOK))
			Expect(auditRows("key.revoke")).To(Equal(1), "revoking twice is one revocation")

			Expect(countRows("SELECT count(*) FROM api_keys WHERE id = $1", issued.ID)).To(Equal(1),
				"revocation is a timestamp, not a delete")
		})

		It("mints a second key when the creation is repeated", func() {
			createTenant()

			first := call("POST", keysPath(), "")
			Expect(first.status).To(Equal(http.StatusCreated), first.body)
			again := call("POST", keysPath(), "")
			Expect(again.status).To(Equal(http.StatusCreated), again.body)

			var a, b struct {
				ID     string
				Secret string
			}
			first.decode(&a)
			again.decode(&b)
			Expect(b.ID).NotTo(Equal(a.ID))
			Expect(b.Secret).NotTo(Equal(a.Secret))

			Expect(countRows("SELECT count(*) FROM api_keys k JOIN tenants t ON t.id = k.tenant_id WHERE t.external_id = $1", external)).
				To(Equal(2))
			Expect(auditRows("key.create")).To(Equal(2))

			c, cancel := ctx()
			defer cancel()
			auth := repositories.NewAuth(pool)
			_, err := auth.Authenticate(c, b.Secret)
			Expect(err).NotTo(HaveOccurred())
			Expect(call("DELETE", "/admin/v1/keys/"+b.ID, "").status).To(Equal(http.StatusOK))
			_, err = auth.Authenticate(c, b.Secret)
			Expect(err).To(MatchError(ports.ErrKeyRevoked))
		})

		It("mints one key per racing request", func() {
			createTenant()

			const racers = 5
			var wg sync.WaitGroup
			ids := make([]string, racers)
			statuses := make([]int, racers)
			for i := range racers {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					r := call("POST", keysPath(), "")
					statuses[i] = r.status
					var k struct{ ID string }
					_ = json.Unmarshal([]byte(r.body), &k)
					ids[i] = k.ID
				}()
			}
			wg.Wait()

			seen := map[string]bool{}
			for i := range racers {
				Expect(statuses[i]).To(Equal(http.StatusCreated))
				Expect(seen[ids[i]]).To(BeFalse(), "two requests were given the same key")
				seen[ids[i]] = true
			}
			Expect(countRows("SELECT count(*) FROM api_keys k JOIN tenants t ON t.id = k.tenant_id WHERE t.external_id = $1", external)).
				To(Equal(racers))
			Expect(auditRows("key.create")).To(Equal(racers))
		})

		It("refuses to mint a key for a disabled tenant", func() {
			createTenant()
			Expect(call("DELETE", "/admin/v1/tenants/"+external, "").status).To(Equal(http.StatusOK))

			r := call("POST", keysPath(), "")
			Expect(r.status).To(Equal(http.StatusConflict), r.body)
		})
	})

	Describe("quotas", func() {
		quotasPath := func() string { return "/admin/v1/tenants/" + external + "/quotas" }

		It("replaces the whole set, and reasserting it changes nothing", func() {
			createTenant()

			Expect(call("PUT", quotasPath(), `{"quotas":[
				{"window_kind":"minute","max_requests":60},
				{"window_kind":"day","max_tokens":100000}
			]}`).status).To(Equal(http.StatusOK))

			set := `{"quotas":[
				{"window_kind":"month","max_cost_nanos":5000000000,"on_exceed":"allow"},
				{"window_kind":"minute","max_requests":120}
			]}`
			r := call("PUT", quotasPath(), set)
			Expect(r.status).To(Equal(http.StatusOK), r.body)

			var got struct {
				Quotas []struct {
					WindowKind string `json:"window_kind"`
				}
			}
			call("GET", quotasPath(), "").decode(&got)
			Expect(got.Quotas).To(HaveLen(2))
			Expect(got.Quotas[0].WindowKind).To(Equal("minute"))
			Expect(got.Quotas[1].WindowKind).To(Equal("month"), "day was left out, so it is gone")

			Expect(call("PUT", quotasPath(), set).status).To(Equal(http.StatusOK))
			Expect(auditRows("quota.replace")).To(Equal(2), "an unchanged set is not a change")
		})

		// The failure is injected by a trigger rather than by bad input,
		// because bad input never reaches the database: this is about what
		// the database is left holding when a write fails part way.
		It("leaves the old set intact when writing the new one fails part way", func() {
			createTenant()
			Expect(call("PUT", quotasPath(), `{"quotas":[{"window_kind":"minute","max_requests":60}]}`).status).
				To(Equal(http.StatusOK))

			c, cancel := ctx()
			defer cancel()
			_, err := pool.Exec(c, `
				CREATE FUNCTION fail_quota_666() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.max_requests = 666 THEN RAISE EXCEPTION 'injected failure'; END IF;
					RETURN NEW;
				END $$;
				CREATE TRIGGER fail_quota_666 BEFORE INSERT ON quotas
					FOR EACH ROW EXECUTE FUNCTION fail_quota_666();`)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				rc, rcancel := ctx()
				defer rcancel()
				_, _ = pool.Exec(rc, "DROP TRIGGER fail_quota_666 ON quotas; DROP FUNCTION fail_quota_666()")
			})

			r := call("PUT", quotasPath(), `{"quotas":[
				{"window_kind":"hour","max_requests":10},
				{"window_kind":"day","max_requests":666}
			]}`)
			Expect(r.status).To(Equal(http.StatusInternalServerError), r.body)

			var got struct {
				Quotas []struct {
					WindowKind  string `json:"window_kind"`
					MaxRequests int64  `json:"max_requests"`
				}
			}
			call("GET", quotasPath(), "").decode(&got)
			Expect(got.Quotas).To(HaveLen(1))
			Expect(got.Quotas[0].WindowKind).To(Equal("minute"))
			Expect(got.Quotas[0].MaxRequests).To(Equal(int64(60)))
			Expect(auditRows("quota.replace")).To(Equal(1), "the failed replace left no audit row")
		})

		It("rejects input the table's constraints would, with a sentence instead", func() {
			createTenant()

			for _, body := range []string{
				`{"quotas":[{"window_kind":"period","max_requests":1}]}`,
				`{"quotas":[{"window_kind":"day","period_start":"2026-09-01T00:00:00Z","period_end":"2026-10-01T00:00:00Z","max_requests":1}]}`,
				`{"quotas":[{"window_kind":"day"}]}`,
				`{"quotas":[{"window_kind":"day","max_tokens":-1}]}`,
				`{"quotas":[{"window_kind":"period","period_start":"2026-10-01T00:00:00Z","period_end":"2026-09-01T00:00:00Z","max_requests":1}]}`,
			} {
				r := call("PUT", quotasPath(), body)
				Expect(r.status).To(Equal(http.StatusBadRequest), body)
				Expect(r.body).NotTo(ContainSubstring("quotas_"), "no constraint names: "+r.body)
			}
		})

		It("sets, reads and deletes a single window", func() {
			createTenant()

			r := call("PUT", quotasPath()+"/period", `{"period_start":"2026-09-14T00:00:00Z","period_end":"2026-10-14T00:00:00Z","max_cost_nanos":1000}`)
			Expect(r.status).To(Equal(http.StatusOK), r.body)
			Expect(call("GET", quotasPath()+"/period", "").status).To(Equal(http.StatusOK))

			Expect(call("DELETE", quotasPath()+"/period", "").status).To(Equal(http.StatusNoContent))
			Expect(call("DELETE", quotasPath()+"/period", "").status).To(Equal(http.StatusNoContent))
			Expect(call("GET", quotasPath()+"/period", "").status).To(Equal(http.StatusNotFound))
			Expect(auditRows("quota.delete")).To(Equal(1))
		})
	})
	Describe("credentials", func() {
		credentialRows := func(action string) int {
			return countRows(
				`SELECT count(*) FROM audit_log WHERE action = $1 AND reason_detail LIKE $2`,
				action, "%"+actorName+"%")
		}

		It("authenticates a minted credential and audits under its name", func() {
			Expect(credential.Secret).To(HavePrefix(admin.AdminKeyPrefix))
			Expect(credential.Secret).To(HavePrefix(credential.Credential.Prefix))

			createTenant()
			Expect(auditRows("tenant.create")).To(Equal(1),
				"the tenant's audit row names the credential, not a self-asserted subject")

			Expect(credentialRows("admin_credential.create")).To(Equal(1))
			Expect(countRows(`SELECT count(*) FROM audit_log
				WHERE action = 'admin_credential.create' AND actor = 'cli:test'`)).To(Equal(1))
		})

		It("records last_used_at out of band", func() {
			Expect(call("GET", "/admin/v1/tenants", "").status).To(Equal(http.StatusOK))

			Eventually(func() int {
				return countRows("SELECT count(*) FROM admin_credentials WHERE id = $1 AND last_used_at IS NOT NULL",
					credential.Credential.ID)
			}, 5*time.Second, 50*time.Millisecond).Should(Equal(1))
		})

		It("stops honouring a revoked credential immediately, with no cache to wait for", func() {
			Expect(call("GET", "/admin/v1/tenants", "").status).To(Equal(http.StatusOK))

			c, cancel := ctx()
			defer cancel()
			revoked, err := creds.Revoke(c, ports.Write{Actor: "cli:test"}, actorName)
			Expect(err).NotTo(HaveOccurred())
			Expect(revoked.RevokedAt).NotTo(BeNil())

			after := call("GET", "/admin/v1/tenants", "")
			Expect(after.status).To(Equal(http.StatusUnauthorized), after.body)
			Expect(after.body).To(ContainSubstring("authentication_error"))
			Expect(after.header.Get("WWW-Authenticate")).To(Equal("Bearer"))

			adminToken = "gr_admin_nevermintedatall"
			unknown := call("GET", "/admin/v1/tenants", "")
			Expect(unknown.status).To(Equal(http.StatusUnauthorized))
			Expect(unknown.body).To(Equal(after.body))

			Expect(credentialRows("admin_credential.revoke")).To(Equal(1))
			Expect(countRows("SELECT count(*) FROM admin_credentials WHERE id = $1", credential.Credential.ID)).
				To(Equal(1), "revocation is a timestamp, not a delete")
		})

		mint := func(role domains.Role, expiresAt *time.Time) ports.IssuedAdminCredential {
			c, cancel := ctx()
			defer cancel()

			issued, err := creds.Create(c, ports.Write{Actor: "cli:test"}, ports.NewAdminCredential{
				Name: actorName + "-" + string(role) + "-" + uuid.NewString(), Role: role, ExpiresAt: expiresAt,
			})
			Expect(err).NotTo(HaveOccurred())
			adminToken = issued.Secret
			return issued
		}

		It("lets a readonly credential see everything and change nothing", func() {
			adminID := createTenant()
			mint(domains.RoleReadonly, nil)

			Expect(call("GET", "/admin/v1/tenants", "").status).To(Equal(http.StatusOK))
			Expect(call("GET", "/admin/v1/tenants/"+adminID, "").status).To(Equal(http.StatusOK))
			Expect(call("GET", "/admin/v1/tenants/"+adminID+"/keys", "").status).To(Equal(http.StatusOK))

			for _, w := range []struct{ method, path, body string }{
				{"POST", "/admin/v1/tenants", `{"external_id":"nope","name":"Nope"}`},
				{"PATCH", "/admin/v1/tenants/" + adminID, `{"name":"Renamed"}`},
				{"DELETE", "/admin/v1/tenants/" + adminID, ""},
				{"POST", "/admin/v1/tenants/" + adminID + "/keys", `{}`},
				{"PUT", "/admin/v1/tenants/" + adminID + "/quotas", `{"quotas":[]}`},
			} {
				r := call(w.method, w.path, w.body)
				Expect(r.status).To(Equal(http.StatusForbidden), w.method+" "+w.path+": "+r.body)
				Expect(r.body).To(ContainSubstring("permission_error"))
				Expect(r.body).To(ContainSubstring("read-only"))
			}

			Expect(countRows("SELECT count(*) FROM tenants WHERE external_id = $1", "nope")).To(Equal(0))
			Expect(auditRows("tenant.update")).To(Equal(0))
		})

		It("refuses an expired credential with the same 401 as an unknown one", func() {
			past := time.Now().Add(-time.Hour)
			expired := mint(domains.RoleAdmin, &past)

			refused := call("GET", "/admin/v1/tenants", "")
			Expect(refused.status).To(Equal(http.StatusUnauthorized), refused.body)
			Expect(refused.header.Get("WWW-Authenticate")).To(Equal("Bearer"))

			adminToken = "gr_admin_nevermintedatall"
			unknown := call("GET", "/admin/v1/tenants", "")
			Expect(unknown.status).To(Equal(http.StatusUnauthorized))
			Expect(unknown.body).To(Equal(refused.body),
				"an expired credential must be indistinguishable from one that never existed")

			Expect(countRows("SELECT count(*) FROM admin_credentials WHERE id = $1 AND last_used_at IS NULL",
				expired.Credential.ID)).To(Equal(1))
		})

		It("honours a credential whose expiry is still ahead of it", func() {
			future := time.Now().Add(time.Hour)
			mint(domains.RoleAdmin, &future)

			Expect(call("GET", "/admin/v1/tenants", "").status).To(Equal(http.StatusOK))
			Expect(call("POST", "/admin/v1/tenants",
				`{"external_id":"`+external+`","name":"Acme"}`).status).To(Equal(http.StatusCreated))
		})

		It("records the role and the expiry a credential was minted with", func() {
			at := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
			issued := mint(domains.RoleReadonly, &at)

			var detail string
			c, cancel := ctx()
			defer cancel()
			Expect(pool.QueryRow(c, `SELECT reason_detail FROM audit_log
				WHERE action = 'admin_credential.create' AND reason_detail LIKE $1`,
				"%"+issued.Credential.ID.String()+"%").Scan(&detail)).To(Succeed())

			Expect(detail).To(ContainSubstring(`"role":"readonly"`))
			Expect(detail).To(ContainSubstring(at.Format("2006-01-02")))

			Expect(countRows(`SELECT count(*) FROM admin_credentials
				WHERE id = $1 AND role = 'readonly' AND expires_at = $2`,
				issued.Credential.ID, at)).To(Equal(1))
		})

		It("refuses a second credential with the same name", func() {
			c, cancel := ctx()
			defer cancel()

			_, err := creds.Create(c, ports.Write{Actor: "cli:test"},
				ports.NewAdminCredential{Name: actorName, Role: domains.RoleAdmin})
			Expect(err).To(MatchError(ports.ErrConflict))

			Expect(countRows("SELECT count(*) FROM admin_credentials WHERE name = $1", actorName)).To(Equal(1))
		})

		It("revokes by id as well as by name, and revoking twice writes one row", func() {
			c, cancel := ctx()
			defer cancel()

			_, err := creds.Revoke(c, ports.Write{Actor: "cli:test"}, credential.Credential.ID.String())
			Expect(err).NotTo(HaveOccurred())
			_, err = creds.Revoke(c, ports.Write{Actor: "cli:test"}, actorName)
			Expect(err).NotTo(HaveOccurred())

			Expect(credentialRows("admin_credential.revoke")).To(Equal(1))

			list, err := creds.List(c)
			Expect(err).NotTo(HaveOccurred())
			Expect(list).To(HaveLen(1))
			Expect(list[0].Revoked()).To(BeTrue())
		})
	})
})
