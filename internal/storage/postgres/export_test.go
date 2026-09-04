package postgres

// Test-only access to the two window loaders.
//
// The SQL LIMITs on these queries bound the DATABASE, and nothing in a run's
// output can see them: Reconcile slices to the same cap whether or not the LIMIT
// is there. The row count each loader returns is the only evidence the bound
// exists, and it is deliberately not exported outside tests -- callers have no
// business with it, but a test that cannot see it cannot tell a real bound from
// a missing one. That is how both LIMITs went untested through four rounds.

var (
	LoadLedgerWindow   = loadLedgerWindow
	LoadUnreconcilable = loadUnreconcilable
)
