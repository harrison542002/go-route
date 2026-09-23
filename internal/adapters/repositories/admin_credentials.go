package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/db/gen"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// AdminCredentialRepo resolves and manages the admin API's own credentials,
// through a pool it does not own.
type AdminCredentialRepo struct {
	q    *gen.Queries
	pool *pgxpool.Pool
}

var _ ports.AdminCredentialRepository = (*AdminCredentialRepo)(nil)

func NewAdminCredentialRepo(pool *pgxpool.Pool) *AdminCredentialRepo {
	return &AdminCredentialRepo{q: gen.New(pool), pool: pool}
}

func (c *AdminCredentialRepo) AdminCredentialByTokenHash(
	ctx context.Context, tokenHash []byte,
) (domains.AdminCredential, error) {
	row, err := c.q.GetAdminCredentialByTokenHash(ctx, tokenHash)
	return adminCredential(row), found(err, "get admin credential")
}

func (c *AdminCredentialRepo) AdminCredentialByRef(ctx context.Context, ref string) (domains.AdminCredential, error) {
	row, err := c.q.GetAdminCredentialByRef(ctx, ref)
	return adminCredential(row), found(err, "get admin credential")
}

func (c *AdminCredentialRepo) ListAdminCredentials(ctx context.Context) ([]domains.AdminCredential, error) {
	rows, err := c.q.ListAdminCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list admin credentials: %w", err)
	}
	out := make([]domains.AdminCredential, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminCredential(r))
	}
	return out, nil
}

func (c *AdminCredentialRepo) TouchAdminCredential(ctx context.Context, id uuid.UUID, at time.Time) error {
	err := c.q.TouchAdminCredential(ctx, gen.TouchAdminCredentialParams{ID: id, UsedAt: &at})
	if err != nil {
		return fmt.Errorf("postgres: touch admin credential: %w", err)
	}
	return nil
}

func (c *AdminCredentialRepo) CreateAdminCredential(
	ctx context.Context, cred domains.AdminCredential, tokenHash []byte, e ports.AuditEntry,
) (domains.AdminCredential, error) {
	var out domains.AdminCredential

	err := c.inTx(ctx, func(q *gen.Queries) error {
		row, err := q.CreateAdminCredential(ctx, gen.CreateAdminCredentialParams{
			ID:          cred.ID,
			Name:        cred.Name,
			TokenHash:   tokenHash,
			TokenPrefix: cred.Prefix,
			Role:        gen.AdminRole(cred.Role),
			ExpiresAt:   cred.ExpiresAt,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: admin credential %q already exists", ports.ErrConflict, cred.Name)
		}
		if err != nil {
			return fmt.Errorf("postgres: create admin credential: %w", err)
		}

		out = adminCredential(row)
		return insertAudit(ctx, q, e)
	})
	if err != nil {
		return domains.AdminCredential{}, err
	}
	return out, nil
}

func (c *AdminCredentialRepo) RevokeAdminCredential(
	ctx context.Context, id uuid.UUID, e ports.AuditEntry,
) (domains.AdminCredential, error) {
	var out domains.AdminCredential

	err := c.inTx(ctx, func(q *gen.Queries) error {
		row, err := q.RevokeAdminCredential(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			// The UPDATE matches nothing when the credential is already revoked.
			// That is a success -- the token is dead either way -- and must not
			// write an audit row for a change that did not happen.
			row, err := q.GetAdminCredentialByRef(ctx, id.String())
			out = adminCredential(row)
			return found(err, "get admin credential")
		}
		if err != nil {
			return fmt.Errorf("postgres: revoke admin credential: %w", err)
		}

		out = adminCredential(row)
		return insertAudit(ctx, q, e)
	})
	if err != nil {
		return domains.AdminCredential{}, err
	}
	return out, nil
}

// inTx wraps the two writes each credential mutation makes -- the change and
// its audit row -- so neither can survive without the other.
func (c *AdminCredentialRepo) inTx(ctx context.Context, fn func(q *gen.Queries) error) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(c.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func adminCredential(r gen.AdminCredential) domains.AdminCredential {
	return domains.AdminCredential{
		ID:         r.ID,
		Name:       r.Name,
		Prefix:     r.TokenPrefix,
		Role:       domains.Role(r.Role),
		ExpiresAt:  r.ExpiresAt,
		CreatedAt:  r.CreatedAt,
		LastUsedAt: r.LastUsedAt,
		RevokedAt:  r.RevokedAt,
	}
}
