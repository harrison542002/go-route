// Package admin is the business side of the internal admin API: what a valid
// tenant, key or quota is, and what each change does when it is made twice.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/ports"
)

type Service struct {
	repo   ports.AdminRepository
	now    func() time.Time
	random io.Reader
}

var _ ports.Admin = (*Service)(nil)

// New builds the service; a nil random means crypto/rand.
func New(repo ports.AdminRepository, now func() time.Time, random io.Reader) *Service {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Service{repo: repo, now: now, random: random}
}

// mutate runs one admin mutation in a transaction, so the change and its audit
// row commit or roll back together. fn must use the context it is handed, not
// the caller's.
func mutate[T any](
	ctx context.Context,
	s *Service,
	fn func(ctx context.Context, tx ports.AdminTx) (T, error),
) (T, error) {
	var out T

	err := s.repo.InTx(ctx, func(ctx context.Context, tx ports.AdminTx) error {
		res, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

func (s *Service) audit(
	ctx context.Context,
	tx ports.AdminTx,
	w ports.Write,
	action string,
	tenantID, keyID *uuid.UUID,
	detail any,
) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("admin: audit id: %w", err)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("admin: encode audit detail: %w", err)
	}
	return tx.Audit().Write(ctx, ports.AuditEntry{
		ID:       id,
		At:       s.now(),
		TenantID: tenantID,
		KeyID:    keyID,
		Actor:    w.Actor,
		Action:   action,
		Detail:   string(raw),
	})
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ports.ErrInvalid, fmt.Sprintf(format, args...))
}

// logAuditFailure reports an audit row that could not be written where a
// louder answer would tell the caller something. It is deliberately not
// an error returned to them.
func logAuditFailure(action string, err error) {
	slog.Error("admin audit row not written", "action", action, "err", err)
}

func newID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("admin: new id: %w", err)
	}
	return id, nil
}
