package repositories

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/harrison542002/go-route/internal/ports"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		code     string
		rejected bool
	}{
		{"23505", true},  // unique_violation
		{"23514", true},  // check_violation, including "no partition for row"
		{"22P02", true},  // invalid_text_representation
		{"08006", false}, // connection_failure
		{"40001", false}, // serialization_failure
		{"42P01", false}, // undefined_table: a deploy problem, retried until fixed
		{"57P01", false}, // admin_shutdown
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			err := classify(fmt.Errorf("postgres: insert: %w", &pgconn.PgError{Code: tt.code}))
			if got := errors.Is(err, ports.ErrRejected); got != tt.rejected {
				t.Errorf("rejected = %v, want %v", got, tt.rejected)
			}
		})
	}

	if errors.Is(classify(context.DeadlineExceeded), ports.ErrRejected) {
		t.Error("a timeout is transient")
	}
}
