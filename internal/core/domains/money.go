package domains

import (
	"fmt"
	"strconv"
)

// USD is an amount in nanodollars (1e-9 USD).
type USD int64

const nanosPerDollar = 1_000_000_000

// FromDollars converts a decimal dollar amount. Intended for config and
// price tables, never for arithmetic on the hot path.
func FromDollars(d float64) USD {
	return USD(d*nanosPerDollar + 0.5)
}

// Dollars returns the amount as a float, for display only. Never feed the
// result back into arithmetic.
func (u USD) Dollars() float64 {
	return float64(u) / nanosPerDollar
}

// String formats to six decimal places: enough to show a fraction of a
// cent, which single requests routinely cost.
func (u USD) String() string {
	return "$" + strconv.FormatFloat(u.Dollars(), 'f', 6, 64)
}

// Format renders with the given number of decimal places, for reports
// where per-request precision would be noise.
func (u USD) Format(decimals int) string {
	return fmt.Sprintf("$%.*f", decimals, u.Dollars())
}

// PerMillionTokens is how providers quote prices: dollars per million
// tokens. Storing the rate at this scale keeps the per-token cost exact.
type PerMillionTokens USD

// Cost returns the price of n tokens at this rate.
func (p PerMillionTokens) Cost(n int) USD {
	return USD(int64(p) * int64(n) / 1_000_000)
}

// formatCost renders a cost at a precision that keeps it readable.
func (u USD) Auto() string {
	d := u.Dollars()
	if d < 0 {
		d = -d
	}

	switch {
	case u == 0:
		return "$0.00"
	case d < 0.01:
		return u.Format(6)
	case d < 1:
		return u.Format(4)
	default:
		return u.Format(2)
	}
}
