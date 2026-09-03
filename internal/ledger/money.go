// Package ledger holds the double-entry domain: accounts, transactions and the
// rules they must obey. It knows nothing about HTTP or SQL.
package ledger

import "fmt"

// Money is an exact amount in an ISO-4217 currency's minor units (cents,
// centavos, pence). Integer arithmetic only -- floating point cannot represent
// most decimal amounts exactly, and rounding error in a ledger is unacceptable.
type Money struct {
	// Amount is in minor units: 12345 in BRL is R$123.45.
	Amount int64
	// Currency is an upper-case ISO-4217 code.
	Currency string
}

func (m Money) String() string {
	return fmt.Sprintf("%d %s", m.Amount, m.Currency)
}

// IsPositive reports whether the amount is strictly greater than zero. A
// transfer of zero moves nothing and is rejected as a client error.
func (m Money) IsPositive() bool { return m.Amount > 0 }

// SameCurrency reports whether two amounts are denominated identically.
func (m Money) SameCurrency(other Money) bool { return m.Currency == other.Currency }

// ValidCurrency reports whether s is shaped like an ISO-4217 alphabetic code.
// This is a format check, not a check against the real currency register: the
// database column is char(3), so anything else is guaranteed to be wrong.
func ValidCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
