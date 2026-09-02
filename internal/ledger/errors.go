package ledger

import "errors"

// Domain errors. Handlers map these to HTTP status codes in one place, so the
// domain never has to know what an HTTP status is.
var (
	// ErrAccountNotFound means a referenced account does not exist.
	ErrAccountNotFound = errors.New("account not found")

	// ErrTransactionNotFound means a referenced transaction does not exist.
	ErrTransactionNotFound = errors.New("transaction not found")

	// ErrCurrencyMismatch means the transfer currency disagrees with one of the
	// accounts. Cross-currency movement needs an FX account and two
	// transactions; it is out of scope for v1.
	ErrCurrencyMismatch = errors.New("currency mismatch")

	// ErrInsufficientFunds means the debited side would breach its balance
	// floor. System accounts opt out via allow_negative_balance.
	ErrInsufficientFunds = errors.New("insufficient funds")

	// ErrSameAccount means both legs name one account, which would be a no-op
	// that still passes the zero-sum check. Almost always a client bug.
	ErrSameAccount = errors.New("debit and credit accounts must differ")

	// ErrDuplicateExternalRef means external_ref is already used by another
	// transaction.
	ErrDuplicateExternalRef = errors.New("external reference already used")

	// ErrInvalidAmount means the amount is zero or negative.
	ErrInvalidAmount = errors.New("amount must be positive")

	// ErrInvalidCurrency means the currency is not a 3-letter ISO-4217 code.
	ErrInvalidCurrency = errors.New("currency must be a 3-letter ISO-4217 code")

	// ErrInvalidAccountKind means the account kind is not one the ledger models.
	ErrInvalidAccountKind = errors.New("invalid account kind")

	// ErrEmptyName means an account was created without a name.
	ErrEmptyName = errors.New("name must not be empty")
)
