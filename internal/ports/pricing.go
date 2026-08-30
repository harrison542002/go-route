package ports

import (
	"errors"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
)

var ErrNoPricing = errors.New("pricing: no rates available")

type PricingTable interface {
	RatesAt(t time.Time, target string) (domains.Rates, string, error)
}
