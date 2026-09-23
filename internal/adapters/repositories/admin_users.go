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

// AdminUserRepo is where the people who reach the admin API live, and the
// stored half of their sessions, through a pool it does not own.
type AdminUserRepo struct {
	q    *gen.Queries
	pool *pgxpool.Pool
}

var _ ports.AdminUserRepository = (*AdminUserRepo)(nil)

func NewAdminUserRepo(pool *pgxpool.Pool) *AdminUserRepo {
	return &AdminUserRepo{q: gen.New(pool), pool: pool}
}

func (r *AdminUserRepo) AdminUserByID(ctx context.Context, id uuid.UUID) (domains.AdminUser, error) {
	row, err := r.q.GetAdminUserByID(ctx, id)
	return adminUser(row), found(err, "get admin user")
}

func (r *AdminUserRepo) AdminUserByEmail(ctx context.Context, email string) (domains.AdminUser, error) {
	row, err := r.q.GetAdminUserByEmail(ctx, email)
	return adminUser(row), found(err, "get admin user")
}

func (r *AdminUserRepo) AdminUserForLogin(
	ctx context.Context, email string,
) (domains.AdminUser, string, error) {
	row, err := r.q.GetAdminUserByEmail(ctx, email)
	if err != nil {
		return domains.AdminUser{}, "", found(err, "get admin user")
	}
	return adminUser(row), row.PasswordHash, nil
}

func (r *AdminUserRepo) ListAdminUsers(ctx context.Context) ([]domains.AdminUser, error) {
	rows, err := r.q.ListAdminUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list admin users: %w", err)
	}
	out := make([]domains.AdminUser, 0, len(rows))
	for _, row := range rows {
		out = append(out, adminUser(row))
	}
	return out, nil
}

func (r *AdminUserRepo) CreateAdminUser(
	ctx context.Context, u domains.AdminUser, passwordHash string, e ports.AuditEntry,
) (domains.AdminUser, error) {
	var out domains.AdminUser

	err := r.inTx(ctx, func(q *gen.Queries) error {
		row, err := q.CreateAdminUser(ctx, gen.CreateAdminUserParams{
			ID:                u.ID,
			Email:             u.Email,
			PasswordHash:      passwordHash,
			Role:              gen.AdminRole(u.Role),
			PasswordChangedAt: u.PasswordChangedAt,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: admin user %q already exists", ports.ErrConflict, u.Email)
		}
		if err != nil {
			return fmt.Errorf("postgres: create admin user: %w", err)
		}

		out = adminUser(row)
		return insertAudit(ctx, q, e)
	})
	if err != nil {
		return domains.AdminUser{}, err
	}
	return out, nil
}

func (r *AdminUserRepo) DisableAdminUser(
	ctx context.Context, id uuid.UUID, at time.Time, e ports.AuditEntry,
) (domains.AdminUser, error) {
	var out domains.AdminUser

	err := r.inTx(ctx, func(q *gen.Queries) error {
		row, err := q.DisableAdminUser(ctx, gen.DisableAdminUserParams{ID: id, DisabledAt: &at})
		if errors.Is(err, pgx.ErrNoRows) {
			// Already disabled: the person is out either way, and an audit
			// row must not claim a change that did not happen.
			row, err := q.GetAdminUserByID(ctx, id)
			out = adminUser(row)
			return found(err, "get admin user")
		}
		if err != nil {
			return fmt.Errorf("postgres: disable admin user: %w", err)
		}
		out = adminUser(row)

		if err := q.RevokeAdminUserRefreshTokens(ctx, gen.RevokeAdminUserRefreshTokensParams{
			UserID: id, RevokedAt: &at,
		}); err != nil {
			return fmt.Errorf("postgres: revoke admin sessions: %w", err)
		}
		return insertAudit(ctx, q, e)
	})
	if err != nil {
		return domains.AdminUser{}, err
	}
	return out, nil
}

func (r *AdminUserRepo) SetAdminUserPassword(
	ctx context.Context, id uuid.UUID, passwordHash string, at time.Time, e ports.AuditEntry,
) (domains.AdminUser, error) {
	var out domains.AdminUser

	err := r.inTx(ctx, func(q *gen.Queries) error {
		row, err := q.SetAdminUserPassword(ctx, gen.SetAdminUserPasswordParams{
			ID: id, PasswordHash: passwordHash, PasswordChangedAt: at,
		})
		if err != nil {
			return found(err, "set admin user password")
		}
		out = adminUser(row)

		// A password change ends the sessions opened under the old one:
		// otherwise changing it after a leak would leave whoever had it a
		// month of refreshes.
		if err := q.RevokeAdminUserRefreshTokens(ctx, gen.RevokeAdminUserRefreshTokensParams{
			UserID: id, RevokedAt: &at,
		}); err != nil {
			return fmt.Errorf("postgres: revoke admin sessions: %w", err)
		}
		return insertAudit(ctx, q, e)
	})
	if err != nil {
		return domains.AdminUser{}, err
	}
	return out, nil
}

func (r *AdminUserRepo) StartAdminSession(
	ctx context.Context, s ports.NewAdminSession, e ports.AuditEntry,
) error {
	return r.inTx(ctx, func(q *gen.Queries) error {
		if err := q.DeleteExpiredAdminRefreshTokens(ctx, gen.DeleteExpiredAdminRefreshTokensParams{
			UserID: s.UserID, Before: s.IssuedAt,
		}); err != nil {
			return fmt.Errorf("postgres: sweep admin sessions: %w", err)
		}
		if err := insertSession(ctx, q, s); err != nil {
			return err
		}
		if err := q.TouchAdminUserLogin(ctx, gen.TouchAdminUserLoginParams{
			ID: s.UserID, At: &s.IssuedAt,
		}); err != nil {
			return fmt.Errorf("postgres: touch admin user login: %w", err)
		}
		return insertAudit(ctx, q, e)
	})
}

func (r *AdminUserRepo) AdminSessionByTokenHash(
	ctx context.Context, tokenHash []byte,
) (ports.AdminSession, error) {
	row, err := r.q.GetAdminRefreshTokenByHash(ctx, tokenHash)
	if err != nil {
		return ports.AdminSession{}, found(err, "get admin session")
	}
	return ports.AdminSession{
		Token: adminRefreshToken(row.AdminRefreshToken),
		User:  adminUser(row.AdminUser),
	}, nil
}

func (r *AdminUserRepo) RotateAdminSession(
	ctx context.Context, presented uuid.UUID, s ports.NewAdminSession,
) error {
	return r.inTx(ctx, func(q *gen.Queries) error {
		if err := insertSession(ctx, q, s); err != nil {
			return err
		}

		// The revoke is conditional on the row still being live, so two
		// refreshes racing on one token cannot both succeed: the loser
		// matches nothing and its whole transaction rolls back, taking the
		// replacement it had just inserted with it.
		_, err := q.RevokeAdminRefreshToken(ctx, gen.RevokeAdminRefreshTokenParams{
			ID: presented, RevokedAt: &s.IssuedAt, ReplacedBy: &s.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: admin refresh token already revoked", ports.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("postgres: rotate admin session: %w", err)
		}
		return nil
	})
}

func (r *AdminUserRepo) EndAdminSession(
	ctx context.Context, id uuid.UUID, at time.Time, e ports.AuditEntry,
) error {
	return r.inTx(ctx, func(q *gen.Queries) error {
		_, err := q.RevokeAdminRefreshToken(ctx, gen.RevokeAdminRefreshTokenParams{ID: id, RevokedAt: &at})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("postgres: end admin session: %w", err)
		}
		return insertAudit(ctx, q, e)
	})
}

func (r *AdminUserRepo) RevokeAdminSessions(
	ctx context.Context, userID uuid.UUID, at time.Time, e ports.AuditEntry,
) error {
	return r.inTx(ctx, func(q *gen.Queries) error {
		if err := q.RevokeAdminUserRefreshTokens(ctx, gen.RevokeAdminUserRefreshTokensParams{
			UserID: userID, RevokedAt: &at,
		}); err != nil {
			return fmt.Errorf("postgres: revoke admin sessions: %w", err)
		}
		return insertAudit(ctx, q, e)
	})
}

func (r *AdminUserRepo) WriteAudit(ctx context.Context, e ports.AuditEntry) error {
	return insertAudit(ctx, r.q, e)
}

func insertSession(ctx context.Context, q *gen.Queries, s ports.NewAdminSession) error {
	_, err := q.CreateAdminRefreshToken(ctx, gen.CreateAdminRefreshTokenParams{
		ID:        s.ID,
		UserID:    s.UserID,
		TokenHash: s.TokenHash,
		IssuedAt:  s.IssuedAt,
		ExpiresAt: s.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("postgres: create admin session: %w", err)
	}
	return nil
}

// inTx wraps the writes each session or user mutation makes -- the change
// and its audit row -- so neither can survive without the other.
func (r *AdminUserRepo) inTx(ctx context.Context, fn func(q *gen.Queries) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(r.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func adminUser(r gen.AdminUser) domains.AdminUser {
	return domains.AdminUser{
		ID:                r.ID,
		Email:             r.Email,
		Role:              domains.Role(r.Role),
		CreatedAt:         r.CreatedAt,
		DisabledAt:        r.DisabledAt,
		LastLoginAt:       r.LastLoginAt,
		PasswordChangedAt: r.PasswordChangedAt,
	}
}

func adminRefreshToken(r gen.AdminRefreshToken) domains.AdminRefreshToken {
	return domains.AdminRefreshToken{
		ID:         r.ID,
		UserID:     r.UserID,
		IssuedAt:   r.IssuedAt,
		ExpiresAt:  r.ExpiresAt,
		RevokedAt:  r.RevokedAt,
		ReplacedBy: r.ReplacedBy,
	}
}
