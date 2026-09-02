package ledger

import (
	"time"

	"github.com/google/uuid"
)

// AccountKind is the classical accounting classification of an account. It
// determines what a debit means: for assets and expenses a debit increases the
// balance, for liabilities, equity and revenue it decreases it.
type AccountKind string

// The five account kinds.
const (
	KindAsset     AccountKind = "asset"
	KindLiability AccountKind = "liability"
	KindEquity    AccountKind = "equity"
	KindRevenue   AccountKind = "revenue"
	KindExpense   AccountKind = "expense"
)

// Valid reports whether k is one of the five modelled kinds.
func (k AccountKind) Valid() bool {
	switch k {
	case KindAsset, KindLiability, KindEquity, KindRevenue, KindExpense:
		return true
	default:
		return false
	}
}

// Account is a balance in a single currency.
type Account struct {
	ID       uuid.UUID
	Name     string
	Kind     AccountKind
	Currency string
	// Balance is in minor units and is maintained transactionally alongside the
	// entries. SUM(ledger_entries.signed_amount) remains the source of truth;
	// reconciliation reports any drift between the two.
	Balance int64
	// AllowNegativeBalance marks a system account -- external funding, fees --
	// that may carry a negative balance. Money has to enter the ledger from
	// somewhere for the global sum to stay zero.
	AllowNegativeBalance bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Money returns the account's balance as a currency-tagged amount.
func (a Account) Money() Money {
	return Money{Amount: a.Balance, Currency: a.Currency}
}

// CreateAccountCommand is the input to creating an account.
type CreateAccountCommand struct {
	Name                 string
	Kind                 AccountKind
	Currency             string
	AllowNegativeBalance bool
}

// Validate checks the command against the domain's rules, returning the first
// violation. Validation lives here rather than in the HTTP layer so every
// caller gets the same rules.
func (c CreateAccountCommand) Validate() error {
	if c.Name == "" {
		return ErrEmptyName
	}
	if !c.Kind.Valid() {
		return ErrInvalidAccountKind
	}
	if !ValidCurrency(c.Currency) {
		return ErrInvalidCurrency
	}
	return nil
}
