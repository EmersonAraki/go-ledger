package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Repository is the persistence the service needs. It is declared here, at the
// point of use, so the domain owns the contract and the storage package depends
// on the domain rather than the other way round.
type Repository interface {
	// CreateAccount persists a new account.
	CreateAccount(ctx context.Context, a *Account) error

	// GetAccount loads one account, returning ErrAccountNotFound when absent.
	GetAccount(ctx context.Context, id uuid.UUID) (*Account, error)

	// Transfer atomically claims the idempotency key, writes the transaction,
	// both entries and both balance updates, and stores the rendered response --
	// locking the two accounts in a deterministic order first. The whole thing
	// commits or none of it does.
	//
	// On a duplicate request it returns the stored response instead of doing the
	// work again, and reports ErrIdempotencyKeyReuse if the key was first used
	// for a different payload.
	Transfer(ctx context.Context, cmd TransferCommand, claim Claim, render RenderFunc) (*Result, error)

	// GetTransaction loads a transaction with its entries, returning
	// ErrTransactionNotFound when absent.
	GetTransaction(ctx context.Context, id uuid.UUID) (*Transaction, error)
}

// Service is the ledger's application layer: it validates commands and
// delegates persistence, so HTTP handlers stay free of business rules.
type Service struct {
	repo Repository
}

// NewService builds a ledger service over the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// CreateAccount validates and stores a new account, which always starts at a
// zero balance -- money only ever arrives through a transaction, so that the
// ledger sums to zero from the very first write.
func (s *Service) CreateAccount(ctx context.Context, cmd CreateAccountCommand) (*Account, error) {
	if err := cmd.Validate(); err != nil {
		return nil, err
	}

	account := &Account{
		ID:                   uuid.New(),
		Name:                 cmd.Name,
		Kind:                 cmd.Kind,
		Currency:             cmd.Currency,
		Balance:              0,
		AllowNegativeBalance: cmd.AllowNegativeBalance,
	}

	if err := s.repo.CreateAccount(ctx, account); err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}
	return account, nil
}

// GetAccount returns one account.
func (s *Service) GetAccount(ctx context.Context, id uuid.UUID) (*Account, error) {
	return s.repo.GetAccount(ctx, id)
}

// Transfer moves money between two accounts, writing both legs of the double
// entry exactly once per idempotency key.
//
// Command-level rules are checked here; the rules that depend on account state
// -- currency agreement and the balance floor -- are enforced inside the
// repository's transaction, after the accounts have been locked. Checking those
// here as well would be a lie: any value read before the lock can be stale by
// the time the write lands.
//
// Validation runs before the claim on purpose. A request that fails validation
// must not consume the key: the client should be able to fix the payload and
// retry with the same key.
func (s *Service) Transfer(ctx context.Context, cmd TransferCommand, claim Claim, render RenderFunc) (*Result, error) {
	if err := cmd.Validate(); err != nil {
		return nil, err
	}

	result, err := s.repo.Transfer(ctx, cmd, claim, render)
	if err != nil {
		return nil, err
	}

	// The database enforces this with a deferred trigger, so reaching here with
	// an unbalanced transaction should be impossible. Assert it anyway: a silent
	// imbalance is the one bug this system must never ship. A replay did no
	// work, so there is nothing to check.
	if result.Transaction != nil && !result.Transaction.Balanced() {
		return nil, fmt.Errorf("internal: transaction %s is not balanced", result.Transaction.ID)
	}
	return result, nil
}

// GetTransaction returns a transaction with its entries.
func (s *Service) GetTransaction(ctx context.Context, id uuid.UUID) (*Transaction, error) {
	return s.repo.GetTransaction(ctx, id)
}
