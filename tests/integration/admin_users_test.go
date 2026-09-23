//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/admin"
)

// adminSigningSecret signs the access tokens these specs log in for. It is a
// fixture, and long enough to pass the floor the binary enforces at startup.
const adminSigningSecret = "0123456789abcdef0123456789abcdef"

type session struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    time.Time
	User         struct {
		ID    string
		Email string
		Role  string
	}
}

var _ = Describe("Admin API sessions", func() {
	var (
		srv     *httptest.Server
		users   *admin.Users
		email   string
		created ports.CreatedAdminUser
	)

	BeforeEach(func() {
		truncate()

		// The first person is made the way the CLI makes them: straight
		// against the database, with no service running and no token held.
		users = admin.NewUsers(repositories.NewAdminUserRepo(pool), nil, 0, time.Now, nil)

		c, cancel := ctx()
		defer cancel()

		email = "ops-" + uuid.NewString() + "@example.com"
		var err error
		created, err = users.Create(c, ports.Write{Actor: "cli:test"},
			ports.NewAdminUser{Email: email, Role: domains.RoleAdmin})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Password).NotTo(BeEmpty())

		srv = httptest.NewServer(builtAdmin().Server.Handler)
		DeferCleanup(srv.Close)
	})

	send := func(method, path, body, bearer string) adminResponse {
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)

		return adminResponse{status: resp.StatusCode, header: resp.Header, body: string(raw)}
	}

	login := func(password string) adminResponse {
		return send("POST", "/admin/v1/auth/login",
			`{"email":"`+email+`","password":`+quote(password)+`}`, "")
	}

	loggedIn := func() session {
		r := login(created.Password)
		Expect(r.status).To(Equal(http.StatusOK), r.body)

		var s session
		r.decode(&s)
		Expect(s.AccessToken).NotTo(BeEmpty())
		Expect(s.RefreshToken).NotTo(BeEmpty())
		return s
	}

	userAudit := func(action string) int {
		return countRows(`SELECT count(*) FROM audit_log WHERE action = $1`, action)
	}

	It("logs in, reaches a guarded endpoint, refreshes, and logs out", func() {
		first := loggedIn()
		Expect(first.User.Email).To(Equal(email))
		Expect(first.User.Role).To(Equal("admin"))
		Expect(userAudit("user.login")).To(Equal(1))

		guarded := send("GET", "/admin/v1/tenants", "", first.AccessToken)
		Expect(guarded.status).To(Equal(http.StatusOK), guarded.body)

		me := send("GET", "/admin/v1/auth/me", "", first.AccessToken)
		Expect(me.status).To(Equal(http.StatusOK), me.body)
		Expect(me.body).To(ContainSubstring(`"actor":"user:` + email + `"`))
		Expect(me.body).To(ContainSubstring(`"kind":"user"`))

		second := send("POST", "/admin/v1/auth/refresh",
			`{"refresh_token":`+quote(first.RefreshToken)+`}`, "")
		Expect(second.status).To(Equal(http.StatusOK), second.body)

		var rotated session
		second.decode(&rotated)
		Expect(rotated.RefreshToken).NotTo(Equal(first.RefreshToken))

		// The presented token is dead the moment it is rotated away, and
		// presenting it again cuts the whole set as a suspected theft.
		replay := send("POST", "/admin/v1/auth/refresh",
			`{"refresh_token":`+quote(first.RefreshToken)+`}`, "")
		Expect(replay.status).To(Equal(http.StatusUnauthorized), replay.body)
		Expect(userAudit("user.refresh_reuse")).To(Equal(1))

		dead := send("POST", "/admin/v1/auth/refresh",
			`{"refresh_token":`+quote(rotated.RefreshToken)+`}`, "")
		Expect(dead.status).To(Equal(http.StatusUnauthorized), dead.body)
	})

	It("ends the session it is asked to end", func() {
		s := loggedIn()

		out := send("POST", "/admin/v1/auth/logout",
			`{"refresh_token":`+quote(s.RefreshToken)+`}`, s.AccessToken)
		Expect(out.status).To(Equal(http.StatusNoContent), out.body)
		Expect(userAudit("user.logout")).To(Equal(1))

		again := send("POST", "/admin/v1/auth/refresh",
			`{"refresh_token":`+quote(s.RefreshToken)+`}`, "")
		Expect(again.status).To(Equal(http.StatusUnauthorized), again.body)
	})

	It("refuses a wrong password with the same answer as an unknown email", func() {
		wrong := login("not-the-password-at-all")
		Expect(wrong.status).To(Equal(http.StatusUnauthorized))

		unknown := send("POST", "/admin/v1/auth/login",
			`{"email":"nobody-`+uuid.NewString()+`@example.com","password":"not-the-password-at-all"}`, "")
		Expect(unknown.status).To(Equal(http.StatusUnauthorized))
		Expect(unknown.body).To(Equal(wrong.body))

		Expect(userAudit("user.login_failed")).To(Equal(2))
		Expect(countRows(
			`SELECT count(*) FROM audit_log WHERE action = 'user.login_failed'
			 AND reason_detail::text LIKE '%not-the-password%'`)).To(Equal(0))
	})

	It("refuses an over-long password with the answer a wrong one gets", func() {
		wrong := login("not-the-password-at-all")
		over := login(strings.Repeat("a", 200))

		Expect(over.status).To(Equal(http.StatusUnauthorized))
		Expect(over.body).To(Equal(wrong.body))
	})

	It("holds a new password to the policy and takes one that satisfies it", func() {
		const (
			refused = "go-route-forever-and-ever"
			chosen  = "a quiet afternoon in the archive"
		)

		c, cancel := ctx()
		defer cancel()

		_, err := users.SetPassword(c, ports.Write{Actor: "cli:test"}, email, refused)
		Expect(err).To(MatchError(ports.ErrInvalid))
		Expect(err.Error()).To(ContainSubstring("name of this product"))
		Expect(login(created.Password).status).To(Equal(http.StatusOK))

		changed, err := users.SetPassword(c, ports.Write{Actor: "cli:test"}, email, chosen)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed.Password).To(BeEmpty(), "a supplied password is never echoed back")

		Expect(login(chosen).status).To(Equal(http.StatusOK))
		Expect(login(created.Password).status).To(Equal(http.StatusUnauthorized))
	})

	It("cuts a disabled person off from the API, not only from the login form", func() {
		s := loggedIn()

		c, cancel := ctx()
		defer cancel()
		_, err := users.Disable(c, ports.Write{Actor: "cli:test"}, email)
		Expect(err).NotTo(HaveOccurred())
		Expect(userAudit("user.disable")).To(Equal(1))

		// The access token is still inside its lifetime and still verifies;
		// the user row is what refuses it.
		guarded := send("GET", "/admin/v1/tenants", "", s.AccessToken)
		Expect(guarded.status).To(Equal(http.StatusUnauthorized), guarded.body)

		refreshed := send("POST", "/admin/v1/auth/refresh",
			`{"refresh_token":`+quote(s.RefreshToken)+`}`, "")
		Expect(refreshed.status).To(Equal(http.StatusUnauthorized), refreshed.body)

		Expect(login(created.Password).status).To(Equal(http.StatusUnauthorized))
	})

	It("applies a demotion to the token the person already holds", func() {
		s := loggedIn()

		c, cancel := ctx()
		defer cancel()
		_, err := pool.Exec(c, `UPDATE admin_users SET role = 'readonly' WHERE email = $1`, email)
		Expect(err).NotTo(HaveOccurred())

		read := send("GET", "/admin/v1/tenants", "", s.AccessToken)
		Expect(read.status).To(Equal(http.StatusOK), read.body)

		write := send("POST", "/admin/v1/tenants",
			`{"external_id":"demoted-`+uuid.NewString()+`","name":"Acme"}`, s.AccessToken)
		Expect(write.status).To(Equal(http.StatusForbidden), write.body)
	})

	It("guards everything but login and refresh", func() {
		for _, path := range []string{"/admin/v1/tenants", "/admin/v1/auth/me"} {
			Expect(send("GET", path, "", "").status).To(Equal(http.StatusUnauthorized), path)
		}
		Expect(send("POST", "/admin/v1/auth/logout", `{"refresh_token":"gr_refresh_x"}`, "").status).
			To(Equal(http.StatusUnauthorized))

		// Reachable without a token: refused on the merits, not on the guard.
		Expect(send("POST", "/admin/v1/auth/refresh", `{"refresh_token":"gr_refresh_x"}`, "").status).
			To(Equal(http.StatusUnauthorized))
		Expect(send("POST", "/admin/v1/auth/login", `{}`, "").status).
			To(Equal(http.StatusBadRequest))
	})
})

func quote(s string) string {
	raw, err := json.Marshal(s)
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}
