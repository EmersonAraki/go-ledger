// Package reconcile compares an external statement against the ledger and
// reports where they disagree.
//
// Reconciliation is strictly read-only against the ledger. It never corrects
// anything: a real correction is a reversing transaction posted through the
// normal API, with its own entries and its own audit trail. A reconciliation job
// that silently adjusted balances would destroy the very property the ledger
// exists to provide.
package reconcile

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Discrepancy kinds. These are reported to operators and stored, so they are
// contract: renaming one breaks saved reports and any dashboard reading them.
const (
	// KindMissingInLedger is a statement row with no corresponding transaction.
	// Money the counterparty says moved that this ledger has no record of.
	KindMissingInLedger = "missing_in_ledger"

	// KindMissingInStatement is a ledger transaction inside the statement's
	// window that the statement does not mention.
	KindMissingInStatement = "missing_in_statement"

	// KindAmountMismatch means the reference matched but the amounts differ.
	KindAmountMismatch = "amount_mismatch"

	// KindCurrencyMismatch means the reference matched but the currencies differ.
	KindCurrencyMismatch = "currency_mismatch"

	// KindAccountMismatch means the reference matched but the money moved
	// between different accounts.
	KindAccountMismatch = "account_mismatch"

	// KindDateMismatch means the reference matched but the posting dates differ
	// by more than the allowed tolerance.
	KindDateMismatch = "date_mismatch"

	// KindDuplicateInStatement means one external reference appears on more than
	// one statement row.
	KindDuplicateInStatement = "duplicate_in_statement"

	// KindUnparseableRow means a row could not be read. Reported rather than
	// aborting the run, so one bad line does not hide every other finding.
	KindUnparseableRow = "unparseable_row"

	// KindProbableMatch is a heuristic pairing made without a shared reference.
	// Reported rather than silently reconciled: a human decides whether two
	// movements that merely look alike are the same one.
	KindProbableMatch = "probable_match"

	// KindFindingsTruncated means there were more findings than one report lists.
	// Distinct from KindStatementTruncated: "we did not read all of your file"
	// and "we did not list all of our findings" are different facts, and a
	// dashboard filtering on one should not silently catch the other.
	KindFindingsTruncated = "findings_truncated"

	// KindUnreconcilableTruncated means the integrity scan examined only part of
	// the window, so a malformed transaction beyond that point was not looked
	// for. A distinct kind rather than a reason string on KindLedgerTruncated:
	// "the comparison is partial" and "the integrity scan is partial" are
	// different facts, and a client filtering on one should not have to parse
	// prose to avoid catching the other.
	KindUnreconcilableTruncated = "unreconcilable_truncated"

	// KindLedgerTruncated means the ledger window held more transactions than one
	// comparison will load, so the comparison is partial.
	KindLedgerTruncated = "ledger_truncated"

	// KindStatementTruncated means the statement exceeded a processing limit and
	// was only partly read. Reported as a finding rather than an error so the
	// partial result is still usable, and so the report can never silently claim
	// to have compared more than it did.
	KindStatementTruncated = "statement_truncated"

	// KindUnreconcilableTransaction is a ledger transaction inside the statement's
	// window with a shape the statement format cannot express -- more than two
	// legs. It is reported rather than skipped: silently excluding it would let
	// the job announce a clean period while money it never examined had moved,
	// which is precisely the failure this job exists to catch.
	KindUnreconcilableTransaction = "unreconcilable_transaction"

	// KindBalanceDrift means an account's cached balance disagrees with the sum
	// of its entries. Not a statement finding at all, and not produced by a
	// statement run: a derived balance is the sum of an account's whole history,
	// so the check is a full sweep and belongs in cmd/driftsweep. It reaches a
	// report through the same tables, under the DriftSweepSource name.
	KindBalanceDrift = "balance_drift"
)

// Processing limits.
//
// The parser reads rows one at a time, but its OUTPUT is proportional to the
// input: an unbounded file yields unbounded slices of rows and findings, each
// far larger than the bytes that produced it. A 32 MiB body of empty comma-only
// lines expands into hundreds of megabytes of retained heap, and there is no
// per-client limit on this endpoint. These caps bound the parsing and the
// reporting. The database side needs its own bounds, and they must be
// expressible in terms of the upload -- a constant sized off the ledger is not a
// bound, it is a bigger ledger's problem deferred. MaxWindowDays caps the span;
// LedgerRowsFor derives the row cap from the number of statement rows actually
// read, so a two-row CSV pays for two rows' worth of ledger rather than for
// whatever the window happens to contain.
//
// A bound must also bound the WORK, not merely the rows returned. A LIMIT above
// an aggregate that filters everything out does neither: the aggregate still
// runs to completion. Every query on this path therefore restricts the set it
// aggregates before aggregating it. A check that cannot be bounded this way does
// not belong on this path at all -- see cmd/driftsweep.
const (
	// MaxStatementRows is the most data rows read from one statement.
	MaxStatementRows = 100_000

	// MaxFindings is the most discrepancies stored and returned for one run.
	// Beyond it the report is truncated: a run with more findings than this is
	// telling the operator something systemic, and listing every instance helps
	// nobody while making the response and the insert unbounded.
	MaxFindings = 10_000

	// MaxWindowDays bounds the period a statement may claim to cover.
	//
	// The window is derived from the file's own dates, so it is attacker
	// controlled: two rows dated 1970 and 2100 make the ledger query load every
	// transaction that has ever existed, from an 80-byte upload. Capping the
	// rows and the findings does not help, because the cost there scales with
	// the DATABASE, not the upload. A statement spanning more than a year is not
	// a statement.
	MaxWindowDays = 366

	// MaxLedgerWindowRows is the ceiling on how much ledger one comparison pulls
	// into memory. It is the last line of defence, not the working bound:
	// LedgerRowsFor is almost always far below it.
	MaxLedgerWindowRows = 200_000

	// LedgerRowsPerStatementRow and MinLedgerWindowRows shape the working bound.
	//
	// The allowance per statement row is generous because a run has to see more
	// ledger than the statement mentions to be useful at all -- that is how
	// missing_in_statement is found. The floor keeps a one-row upload from
	// reporting a truncated window for a ledger that is nearly empty.
	LedgerRowsPerStatementRow = 100
	MinLedgerWindowRows       = 1_000

	// MaxReportedParseErrors is the most individual unparseable-row findings
	// collected. Beyond it the remainder are counted, not listed: a file that is
	// wrong on every line needs one clear answer, not a hundred thousand copies
	// of it.
	MaxReportedParseErrors = 1_000
)

// ErrWindowTooWide means the statement claims to span an implausible period.
var ErrWindowTooWide = errors.New("statement window is too wide")

// Discrepancy is one disagreement worth an operator's attention.
type Discrepancy struct {
	// ID is the stored row's identifier, assigned on save. It is what
	// pagination cursors are expressed in: the ids come from a shared sequence,
	// so the Nth finding of a run is NOT row id N, and a cursor that conflates
	// the two makes a client re-receive findings it already has.
	ID   int64
	Kind string
	// StatementRef is the external reference involved, when there is one.
	StatementRef *string
	// TransactionID is the ledger transaction involved, when there is one.
	TransactionID *uuid.UUID
	// Details carries the specifics -- expected versus actual -- as free-form
	// JSON, because what matters differs per kind.
	Details map[string]any
}

// Run is the result of one reconciliation.
type Run struct {
	ID               uuid.UUID
	SourceName       string
	StatementRows    int
	MatchedCount     int
	DiscrepancyCount int
	// WindowStart and WindowEnd bound the period the statement covers, derived
	// from its rows. Ledger transactions outside it are not this statement's
	// business and are not reported as missing.
	WindowStart *time.Time
	WindowEnd   *time.Time
	CreatedAt   time.Time

	Discrepancies []Discrepancy
}

// Clean reports whether the statement and the ledger agreed completely.
func (r Run) Clean() bool { return r.DiscrepancyCount == 0 }

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }

// LedgerRowsFor is the row cap for one run, derived from the statement rows
// actually read. This is the bound the plan requires: expressible in terms of
// the upload, so the cost of a request tracks the size of the file rather than
// the size of the database.
//
// Both window queries share it. A run that examined the first N transactions of
// a window did so for the comparison and for the integrity scan alike, and two
// different caps would only make the report harder to reason about.
func LedgerRowsFor(statementRows int) int {
	if statementRows < 0 {
		statementRows = 0
	}
	// Guard the multiplication before it can overflow on a hostile row count.
	if statementRows > MaxLedgerWindowRows {
		return MaxLedgerWindowRows
	}
	return min(LedgerRowsPerStatementRow*statementRows+MinLedgerWindowRows, MaxLedgerWindowRows)
}
