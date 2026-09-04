-- Reconciliation selects transactions by their posting time, and that is the
-- only predicate it has. Without this index every uploaded statement
-- sequentially scans the whole transactions table, inside a request deadline.
CREATE INDEX transactions_created_at_idx ON transactions (created_at);
