// Package redisquota keeps live quota counters in Redis.
//
// Keyspace: one HASH per tenant per window,
//
//	q:{<tenant uuid>}:<window kind>:<window start, unix seconds>
//
// with fields requests, tokens and cost_nanos, expiring at the window's end
// plus a grace period. The braces are a Redis Cluster hash tag: every key of one
// tenant hashes to the same slot, which is what allows one script to touch all
// of a tenant's windows atomically on a cluster as well as on a single node.
package redisquota

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// expiryGrace keeps a counter past its window's end, because reconciliation
// adjusts the windows a request was reserved in and a long stream finishes well
// after its minute is over. Without it the key would be gone and the correction
// lost.
const expiryGrace = time.Hour

//go:embed reserve.lua
var reserveSrc string

//go:embed seed.lua
var seedSrc string

//go:embed adjust.lua
var adjustSrc string

// redis.NewScript runs EVALSHA and falls back to EVAL when the server does not
// have the script cached, as after a restart or failover.
var (
	reserveScript = redis.NewScript(reserveSrc)
	seedScript    = redis.NewScript(seedSrc)
	adjustScript  = redis.NewScript(adjustSrc)
)

// ErrUnexpectedReply means a script answered in a shape this code does not
// know, which is a deployment mismatch rather than an outage.
var ErrUnexpectedReply = errors.New("redisquota: unexpected script reply")

// Counters implements ports.QuotaCounters.
type Counters struct {
	client  redis.Scripter
	timeout time.Duration
}

var _ ports.QuotaCounters = (*Counters)(nil)

// New wraps a client. timeout bounds every call, so an unreachable Redis costs
// a request that long and no longer.
func New(client redis.Scripter, timeout time.Duration) *Counters {
	return &Counters{
		client:  client,
		timeout: timeout,
	}
}

// Key is the counter for one window of one tenant.
func Key(tenantID uuid.UUID, w domains.Window) string {
	return fmt.Sprintf("q:{%s}:%s:%d", tenantID, w.Kind, w.Start.Unix())
}

func expireAt(w domains.Window) int64 {
	return w.End.Add(expiryGrace).Unix()
}

func (c *Counters) Reserve(
	ctx context.Context,
	tenantID uuid.UUID,
	limits []domains.WindowLimit,
	amount domains.QuotaUsage,
) (ports.CounterReservation, error) {
	keys := make([]string, len(limits))
	args := make([]any, 0, 3+len(limits)*3)
	args = append(args, amount.Requests, amount.Tokens, int64(amount.Cost))

	for i, l := range limits {
		keys[i] = Key(tenantID, l.Window)

		block := 0
		if l.Block {
			block = 1
		}
		args = append(args, expireAt(l.Window), block, int64(l.MaxCost))
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	reply, err := reserveScript.Run(ctx, c.client, keys, args...).Slice()
	if err != nil {
		return ports.CounterReservation{}, fmt.Errorf("redisquota: reserve: %w", err)
	}
	return parseReserve(reply, limits, amount)
}

// parseReserve decodes the script's reply:
//
//	{"missing", i, ...}             windows i have no counter
//	{"blocked", i, used, limit}     nothing reserved
//	{"ok", i, used, limit, ...}     reserved; soft breaches follow
//
// Indices are 1-based, as Lua counts.
func parseReserve(reply []any, limits []domains.WindowLimit, amount domains.QuotaUsage) (ports.CounterReservation, error) {
	if len(reply) == 0 {
		return ports.CounterReservation{}, ErrUnexpectedReply
	}
	status, _ := reply[0].(string)
	rest := reply[1:]

	window := func(v any) (domains.Window, error) {
		i, ok := v.(int64)
		if !ok || i < 1 || int(i) > len(limits) {
			return domains.Window{}, fmt.Errorf("%w: window index %v", ErrUnexpectedReply, v)
		}
		return limits[i-1].Window, nil
	}

	breaches := func(vals []any) ([]domains.QuotaBreach, error) {
		if len(vals)%3 != 0 {
			return nil, fmt.Errorf("%w: %d breach fields", ErrUnexpectedReply, len(vals))
		}
		out := make([]domains.QuotaBreach, 0, len(vals)/3)
		for i := 0; i < len(vals); i += 3 {
			w, err := window(vals[i])
			if err != nil {
				return nil, err
			}
			used, _ := vals[i+1].(int64)
			limit, _ := vals[i+2].(int64)
			out = append(out, domains.QuotaBreach{
				Window: w, Used: domains.USD(used), Limit: domains.USD(limit), Requested: amount.Cost,
			})
		}
		return out, nil
	}

	switch status {
	case "missing":
		missing := make([]domains.Window, 0, len(rest))
		for _, v := range rest {
			w, err := window(v)
			if err != nil {
				return ports.CounterReservation{}, err
			}
			missing = append(missing, w)
		}
		return ports.CounterReservation{Missing: missing}, nil

	case "blocked":
		b, err := breaches(rest)
		if err != nil || len(b) != 1 {
			return ports.CounterReservation{}, fmt.Errorf("%w: blocked reply %v", ErrUnexpectedReply, reply)
		}
		return ports.CounterReservation{Blocked: &b[0]}, nil

	case "ok":
		soft, err := breaches(rest)
		if err != nil {
			return ports.CounterReservation{}, err
		}
		return ports.CounterReservation{Soft: soft}, nil

	default:
		return ports.CounterReservation{}, fmt.Errorf("%w: status %q", ErrUnexpectedReply, status)
	}
}

// Seed runs one script call per tenant: keys of different tenants hash to
// different slots, and a script may only touch one.
func (c *Counters) Seed(ctx context.Context, seeds []domains.WindowUsage) error {
	byTenant := make(map[uuid.UUID][]domains.WindowUsage)
	for _, s := range seeds {
		byTenant[s.TenantID] = append(byTenant[s.TenantID], s)
	}

	for tenantID, group := range byTenant {
		if err := c.seedTenant(ctx, tenantID, group); err != nil {
			return err
		}
	}
	return nil
}

func (c *Counters) seedTenant(ctx context.Context, tenantID uuid.UUID, seeds []domains.WindowUsage) error {
	keys := make([]string, len(seeds))
	args := make([]any, 0, len(seeds)*4)
	for i, s := range seeds {
		keys[i] = Key(tenantID, s.Window)
		args = append(args, s.Usage.Requests, s.Usage.Tokens, int64(s.Usage.Cost), expireAt(s.Window))
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if err := seedScript.Run(ctx, c.client, keys, args...).Err(); err != nil {
		return fmt.Errorf("redisquota: seed: %w", err)
	}
	return nil
}

func (c *Counters) Adjust(ctx context.Context, tenantID uuid.UUID, windows []domains.Window, delta domains.QuotaUsage) error {
	if len(windows) == 0 || delta.IsZero() {
		return nil
	}

	keys := make([]string, len(windows))
	for i, w := range windows {
		keys[i] = Key(tenantID, w)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	err := adjustScript.Run(ctx, c.client, keys,
		strconv.FormatInt(delta.Requests, 10),
		strconv.FormatInt(delta.Tokens, 10),
		strconv.FormatInt(int64(delta.Cost), 10)).Err()
	if err != nil {
		return fmt.Errorf("redisquota: adjust: %w", err)
	}
	return nil
}
