//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/admin"
)

// The dashboard-facing reads, end to end: the listener bootstrap builds for the
// binary, over rows written by the real sink.
var _ = Describe("Admin API observability", func() {
	var (
		srv   *httptest.Server
		token string
		store *repositories.ObservabilityRepo
		write func(...domains.RoutingDecision)
	)

	const tenant = "acme"

	BeforeEach(func() {
		truncate()

		c, cancel := ctx()
		defer cancel()

		creds := admin.NewCredentials(repositories.NewAdminCredentialRepo(pool), time.Now, nil)
		issued, err := creds.Create(c, ports.Write{Actor: "cli:test"},
			ports.NewAdminCredential{Name: "obs-" + uuid.NewString(), Role: domains.RoleReadonly})
		Expect(err).NotTo(HaveOccurred())
		token = issued.Secret

		srv = httptest.NewServer(builtAdmin().Server.Handler)
		DeferCleanup(srv.Close)

		store, err = repositories.NewObservabilityRepo(c, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(store.Close)

		writer := repositories.NewRecordWriter(pool)
		write = func(batch ...domains.RoutingDecision) {
			w, wcancel := ctx()
			defer wcancel()
			Expect(writer.Write(w, batch)).To(Succeed())
		}
	})

	get := func(path string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	window := "since=" + url.QueryEscape(testTime.Add(-24*time.Hour).Format(time.RFC3339)) +
		"&until=" + url.QueryEscape(testTime.Add(24*time.Hour).Format(time.RFC3339))

	Describe("usage", func() {
		It("reports what the store aggregates", func() {
			write(
				newDecision(withTenant(tenant)),
				newDecision(withTenant(tenant)),
				newDecision(withTenant(tenant), withMetadata(map[string]string{"feature": "summariser"})),
				newDecision(withTenant(tenant), unpriced()),
				newDecision(withTenant(tenant), exhausted()),
			)

			c, cancel := ctx()
			defer cancel()
			want, err := store.Aggregate(c, ports.ReportSpec{
				Tenant:  tenant,
				Since:   testTime.Add(-24 * time.Hour),
				Until:   testTime.Add(24 * time.Hour),
				GroupBy: ports.GroupByMetadata,
				MetaKey: "feature",
			})
			Expect(err).NotTo(HaveOccurred())

			status, body := get("/admin/v1/tenants/" + tenant + "/usage?" + window +
				"&group_by=metadata&meta_key=feature")
			Expect(status).To(Equal(http.StatusOK), body)

			var got struct {
				Spec struct {
					Tenant  string `json:"tenant"`
					GroupBy string `json:"group_by"`
					MetaKey string `json:"meta_key"`
				} `json:"spec"`
				Rows []struct {
					Key         string `json:"key"`
					Requests    int64  `json:"requests"`
					CostNanos   int64  `json:"cost_nanos"`
					Unpriced    int64  `json:"unpriced"`
					Ok          int64  `json:"ok"`
					Failed      int64  `json:"failed"`
					Comparisons []struct {
						Target    string `json:"target"`
						CostNanos int64  `json:"cost_nanos"`
					} `json:"comparisons"`
				} `json:"rows"`
				Total struct {
					Requests  int64 `json:"requests"`
					CostNanos int64 `json:"cost_nanos"`
					Unpriced  int64 `json:"unpriced"`
				} `json:"total"`
			}
			Expect(json.Unmarshal([]byte(body), &got)).To(Succeed())

			Expect(got.Spec.Tenant).To(Equal(tenant))
			Expect(got.Spec.GroupBy).To(Equal("metadata"))
			Expect(got.Spec.MetaKey).To(Equal("feature"))

			Expect(got.Rows).To(HaveLen(len(want.Rows)))
			byKey := map[string]int64{}
			for _, row := range got.Rows {
				byKey[row.Key] = row.CostNanos
				Expect(row.Requests).To(BeNumerically(">", 0))
			}
			for _, row := range want.Rows {
				Expect(byKey).To(HaveKeyWithValue(row.Key, int64(row.Cost)))
			}

			Expect(got.Total.Requests).To(Equal(want.Total.Requests))
			Expect(got.Total.CostNanos).To(Equal(int64(want.Total.Cost)))
			Expect(got.Total.Unpriced).To(Equal(want.Total.Unpriced))

			var compared bool
			for _, row := range got.Rows {
				for _, cf := range row.Comparisons {
					if cf.Target == "openai/gpt-5" {
						Expect(cf.CostNanos).To(BeNumerically(">", 0))
						compared = true
					}
				}
			}
			Expect(compared).To(BeTrue(), body)
		})

		It("is 404 for a tenant nobody created", func() {
			status, body := get("/admin/v1/tenants/never-existed/usage?" + window)
			Expect(status).To(Equal(http.StatusNotFound), body)
		})
	})

	Describe("audit paging", func() {
		It("walks every event once, with no gaps and no repeats", func() {
			const total = 25

			batch := make([]domains.RoutingDecision, 0, total)
			for i := range total {
				at := testTime.Add(time.Duration(i/5) * time.Minute)
				batch = append(batch, newDecision(withTenant(tenant), withTime(at)))
			}
			write(batch...)

			want := map[string]bool{}
			for _, d := range batch {
				want[d.ID.UUID().String()] = false
			}

			type page struct {
				Events []struct {
					ID string `json:"id"`
					At string `json:"at"`
				} `json:"events"`
				NextCursor *string `json:"next_cursor"`
			}

			seen := map[string]int{}
			var order []time.Time
			cursor := ""
			for pages := 0; ; pages++ {
				Expect(pages).To(BeNumerically("<", 20), "paging did not terminate")

				path := "/admin/v1/tenants/" + tenant + "/audit?" + window + "&limit=7"
				if cursor != "" {
					path += "&cursor=" + url.QueryEscape(cursor)
				}
				status, body := get(path)
				Expect(status).To(Equal(http.StatusOK), body)

				var p page
				Expect(json.Unmarshal([]byte(body), &p)).To(Succeed())
				for _, e := range p.Events {
					seen[e.ID]++
					at, err := time.Parse(time.RFC3339, e.At)
					Expect(err).NotTo(HaveOccurred(), "event timestamp "+e.At)
					order = append(order, at)
				}
				if p.NextCursor == nil {
					break
				}
				Expect(p.Events).To(HaveLen(7), "a page with more to come must be full")
				cursor = *p.NextCursor
			}

			Expect(seen).To(HaveLen(total), "pages did not cover every event exactly once")
			for id := range want {
				Expect(seen).To(HaveKeyWithValue(id, 1), "event "+id+" was missed or repeated")
			}

			for i := 1; i < len(order); i++ {
				Expect(order[i].After(order[i-1])).To(BeFalse(),
					"events must be newest first, across page boundaries as well as inside one")
			}
		})

		It("refuses a cursor it did not issue", func() {
			status, body := get("/admin/v1/tenants/" + tenant + "/audit?" + window + "&cursor=nonsense")
			Expect(status).To(Equal(http.StatusBadRequest), body)
			Expect(body).To(ContainSubstring("invalid_request_error"))
		})
	})

	Describe("one request", func() {
		It("returns the ladder and every attempt", func() {
			d := newDecision(withTenant(tenant), exhausted())
			write(d)

			status, body := get("/admin/v1/requests/" + d.ID.String())
			Expect(status).To(Equal(http.StatusOK), body)

			var got struct {
				ID     string `json:"id"`
				Tenant string `json:"tenant"`
				Status string `json:"status"`
				Ladder struct {
					ReasonKind string `json:"reason_kind"`
					ModelAlias string `json:"model_alias"`
					Targets    []struct {
						Name string `json:"name"`
					} `json:"targets"`
				} `json:"ladder"`
				Attempts []struct {
					Target  string `json:"target"`
					Failure *struct {
						Kind    string `json:"kind"`
						Message string `json:"message"`
					} `json:"failure"`
				} `json:"attempts"`
				ChosenTarget *string `json:"chosen_target"`
			}
			Expect(json.Unmarshal([]byte(body), &got)).To(Succeed())

			Expect(got.ID).To(Equal(d.ID.String()))
			Expect(got.Tenant).To(Equal(tenant))
			Expect(got.Status).To(Equal(string(domains.StatusExhausted)))
			Expect(got.Ladder.ReasonKind).To(Equal(string(domains.ReasonModelAlias)))
			Expect(got.Ladder.ModelAlias).To(Equal("chat"))
			Expect(got.Ladder.Targets).NotTo(BeEmpty())

			Expect(got.Attempts).To(HaveLen(1))
			Expect(got.Attempts[0].Failure).NotTo(BeNil())
			Expect(got.Attempts[0].Failure.Message).To(Equal("connection refused"))
			Expect(got.ChosenTarget).To(BeNil(), "nothing served an exhausted request")
		})

		It("says a missing decision may simply have aged out", func() {
			status, body := get("/admin/v1/requests/" + domains.NewDecisionID().String())
			Expect(status).To(Equal(http.StatusNotFound), body)
			Expect(body).To(ContainSubstring("aged out"))
		})
	})
})
