package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EmersonAraki/go-ledger/internal/ledger"
)

// Store is the PostgreSQL implementation of ledger.Repository. It is the only
// type in the codebase that writes ledger SQL.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a store over a connection pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateAccount inserts a new account.
func (s *Store) CreateAccount(ctx context.Context, a *ledger.Account) error {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO accounts (id, name, kind, currency, balance, allow_negative_balance)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`,
		a.ID, a.Name, a.Kind, a.Currency, a.Balance, a.AllowNegativeBalance,
	).Scan(&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert account: %w", err)
	}
	return nil
}

// GetAccount loads one account by id.
func (s *Store) GetAccount(ctx context.Context, id uuid.UUID) (*ledger.Account, error) {
	var a ledger.Account
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, kind, currency, balance, allow_negative_balance, created_at, updated_at
		  FROM accounts
		 WHERE id = $1`, id,
	).Scan(&a.ID, &a.Name, &a.Kind, &a.Currency, &a.Balance,
		&a.AllowNegativeBalance, &a.CreatedAt, &a.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ledger.ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select account: %w", err)
	}
	return &a, nil
}

// Transfer writes a two-leg transaction and both balance updates atomically.
//
// Isolation: READ COMMITTED plus an explicit row lock on the two accounts,
// taken in ascending id order. Ordering the lock acquisition is what makes
// concurrent A->B and B->A transfers deadlock-free rather than a coin flip.
// REPEATABLE READ is deliberately not used here: in PostgreSQL that is snapshot
// isolation, which does not prevent the write skew that lets two concurrent
// withdrawals overdraw one account. See docs/IMPLEMENTATION_PLAN.md section 7.1.
func (s *Store) Transfer(ctx context.Context, cmd ledger.TransferCommand) (*ledger.Transaction, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin transfer: %w", err)
	}
	// Rolled back unless the commit below succeeds. WithoutCancel so that a
	// cancelled request still releases its locks promptly.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	accounts, err := lockAccounts(ctx, tx, cmd.DebitAccountID, cmd.CreditAccountID)
	if err != nil {
		return nil, err
	}
	debit, credit := accounts[cmd.DebitAccountID], accounts[cmd.CreditAccountID]

	// Currency must agree across both accounts and the transfer itself.
	// Cross-currency movement is out of scope for v1.
	if debit.Currency != cmd.Currency || credit.Currency != cmd.Currency {
		return nil, fmt.Errorf("%w: transfer in %s between %s and %s accounts",
			ledger.ErrCurrencyMismatch, cmd.Currency, debit.Currency, credit.Currency)
	}

	// The funding side must be able to cover the movement. The accounts are
	// locked, so this balance is current and cannot change under us before the
	// commit. accounts_balance_floor is the database-level backstop.
	if !credit.AllowNegativeBalance && credit.Balance < cmd.Amount {
		return nil, fmt.Errorf("%w: account %s holds %d, needs %d",
			ledger.ErrInsufficientFunds, credit.ID, credit.Balance, cmd.Amount)
	}

	result, err := writeTransaction(ctx, tx, cmd)
	if err != nil {
		return nil, err
	}

	if err := applyBalances(ctx, tx, cmd); err != nil {
		return nil, err
	}

	// The zero-sum trigger is DEFERRABLE INITIALLY DEFERRED, so an imbalance
	// surfaces here, at COMMIT, rather than on the individual inserts.
	if err := tx.Commit(ctx); err != nil {
		return nil, translateWriteError(ctx, err)
	}
	return result, nil
}

// lockAccounts loads the given accounts FOR UPDATE, ordered by id so that
// concurrent transfers between the same pair cannot deadlock.
func lockAccounts(ctx context.Context, tx pgx.Tx, ids ...uuid.UUID) (map[uuid.UUID]*ledger.Account, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, name, kind, currency, balance, allow_negative_balance, created_at, updated_at
		  FROM accounts
		 WHERE id = ANY($1)
		 ORDER BY id
		   FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	defer rows.Close()

	found := make(map[uuid.UUID]*ledger.Account, len(ids))
	for rows.Next() {
		var a ledger.Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &a.Currency, &a.Balance,
			&a.AllowNegativeBalance, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan locked account: %w", err)
		}
		found[a.ID] = &a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read locked accounts: %w", err)
	}

	// Name the missing account rather than reporting a bare "not found".
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			return nil, fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, id)
		}
	}
	return found, nil
}

// writeTransaction inserts the transaction row and its two entries.
func writeTransaction(ctx context.Context, tx pgx.Tx, cmd ledger.TransferCommand) (*ledger.Transaction, error) {
	result := &ledger.Transaction{
		ID:          uuid.New(),
		ExternalRef: cmd.ExternalRef,
		Description: cmd.Description,
		Currency:    cmd.Currency,
	}

	err := tx.QueryRow(ctx, `
		INSERT INTO transactions (id, external_ref, description, currency)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`,
		result.ID, cmd.ExternalRef, cmd.Description, cmd.Currency,
	).Scan(&result.CreatedAt)
	if err != nil {
		return nil, translateWriteError(ctx, fmt.Errorf("insert transaction: %w", err))
	}

	legs := []struct {
		account   uuid.UUID
		direction ledger.Direction
	}{
		{cmd.DebitAccountID, ledger.Debit},
		{cmd.CreditAccountID, ledger.Credit},
	}

	for _, leg := range legs {
		entry := ledger.Entry{
			ID:            uuid.New(),
			TransactionID: result.ID,
			AccountID:     leg.account,
			Direction:     leg.direction,
			Amount:        cmd.Amount,
			Currency:      cmd.Currency,
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO ledger_entries (id, transaction_id, account_id, direction, amount, currency)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING created_at`,
			entry.ID, entry.TransactionID, entry.AccountID,
			entry.Direction, entry.Amount, entry.Currency,
		).Scan(&entry.CreatedAt)
		if err != nil {
			return nil, translateWriteError(ctx, fmt.Errorf("insert %s entry: %w", leg.direction, err))
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}

// applyBalances moves the money. The debited account gains the amount and the
// crediting account funds it, which is what keeps the cached balances in step
// with SUM(signed_amount).
func applyBalances(ctx context.Context, tx pgx.Tx, cmd ledger.TransferCommand) error {
	_, err := tx.Exec(ctx, `
		UPDATE accounts
		   SET balance = balance + CASE WHEN id = $1 THEN $3::bigint ELSE -$3::bigint END,
		       updated_at = now()
		 WHERE id IN ($1, $2)`,
		cmd.DebitAccountID, cmd.CreditAccountID, cmd.Amount)
	if err != nil {
		return translateWriteError(ctx, fmt.Errorf("apply balances: %w", err))
	}
	return nil
}

// GetTransaction loads a transaction and its entries.
func (s *Store) GetTransaction(ctx context.Context, id uuid.UUID) (*ledger.Transaction, error) {
	var t ledger.Transaction
	err := s.pool.QueryRow(ctx, `
		SELECT id, external_ref, description, currency, created_at
		  FROM transactions
		 WHERE id = $1`, id,
	).Scan(&t.ID, &t.ExternalRef, &t.Description, &t.Currency, &t.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ledger.ErrTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select transaction: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, transaction_id, account_id, direction, amount, currency, created_at
		  FROM ledger_entries
		 WHERE transaction_id = $1
		 ORDER BY direction DESC, id`, id)
	if err != nil {
		return nil, fmt.Errorf("select entries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var e ledger.Entry
		if err := rows.Scan(&e.ID, &e.TransactionID, &e.AccountID,
			&e.Direction, &e.Amount, &e.Currency, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		t.Entries = append(t.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read entries: %w", err)
	}
	return &t, nil
}

// translateWriteError converts the constraint violations the schema is designed
// to raise into domain errors, so callers match on meaning rather than on
// PostgreSQL error codes.
//
// The raw driver error is deliberately NOT wrapped into the returned chain: the
// HTTP layer puts err.Error() into the problem+json "detail" field, so anything
// left in the message reaches the client. Constraint names and SQL fragments are
// internal detail. They are logged here instead, where an operator can see them.
func translateWriteError(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	var domainErr error
	switch pgErr.ConstraintName {
	case "transactions_external_ref_key":
		domainErr = ledger.ErrDuplicateExternalRef
	case "ledger_entries_amount_check":
		domainErr = ledger.ErrInvalidAmount
	case "accounts_balance_floor":
		// The service checks the floor against locked rows before writing, so
		// reaching this backstop means the application logic and the schema
		// disagree. That is a bug worth shouting about, even though the client
		// still just sees "insufficient funds".
		slog.ErrorContext(ctx, "balance floor breached despite pre-check; application logic and schema disagree",
			"error", err, "constraint", pgErr.ConstraintName)
		return ledger.ErrInsufficientFunds
	default:
		return err
	}

	slog.WarnContext(ctx, "constraint violation translated to domain error",
		"error", err, "constraint", pgErr.ConstraintName, "domain_error", domainErr)
	return domainErr
}
