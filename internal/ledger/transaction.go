package ledger

import (
	"time"

	"github.com/google/uuid"
)

// Direction is which side of the ledger an entry falls on.
type Direction string

// The two entry directions.
const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Entry is one leg of a transaction: an amount applied to a single account.
// Entries are append-only; a mistake is corrected by a reversing transaction,
// never by editing history.
type Entry struct {
	ID            uuid.UUID
	TransactionID uuid.UUID
	AccountID     uuid.UUID
	Direction     Direction
	// Amount is always positive. Direction carries the sign.
	Amount    int64
	Currency  string
	CreatedAt time.Time
}

// SignedAmount is the amount as it contributes to the zero-sum check: debits
// positive, credits negative. It mirrors the generated column of the same name.
func (e Entry) SignedAmount() int64 {
	if e.Direction == Debit {
		return e.Amount
	}
	return -e.Amount
}

// Transaction is a balanced set of entries applied atomically.
type Transaction struct {
	ID          uuid.UUID
	ExternalRef *string
	Description string
	Currency    string
	CreatedAt   time.Time
	Entries     []Entry
}

// Balanced reports whether the entries sum to zero, which is the invariant the
// whole ledger rests on. The database enforces this independently with a
// deferred constraint trigger; this method exists so the service can fail early
// with a clear domain error instead of surfacing a raw constraint violation.
func (t Transaction) Balanced() bool {
	var sum int64
	for _, e := range t.Entries {
		sum += e.SignedAmount()
	}
	return sum == 0
}

// TransferCommand moves money between two accounts.
//
// Direction convention: DebitAccountID is credited with the money in the
// everyday sense -- its signed total goes up by Amount -- and CreditAccountID
// funds the movement, going down by the same amount. The names follow
// accounting, not banking: money flows from the credited account to the debited
// one.
type TransferCommand struct {
	DebitAccountID  uuid.UUID
	CreditAccountID uuid.UUID
	Amount          int64
	Currency        string
	Description     string
	ExternalRef     *string
}

// Validate checks the command in isolation. Rules that need the accounts
// themselves -- currency agreement, sufficient funds -- are enforced by the
// service once it has loaded and locked them.
func (c TransferCommand) Validate() error {
	if c.Amount <= 0 {
		return ErrInvalidAmount
	}
	if !ValidCurrency(c.Currency) {
		return ErrInvalidCurrency
	}
	if c.DebitAccountID == c.CreditAccountID {
		return ErrSameAccount
	}
	return nil
}
