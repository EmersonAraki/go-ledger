-- Store the idempotent response body verbatim.
--
-- jsonb is a parsed binary representation: it sorts object keys, collapses
-- whitespace and drops duplicate keys. That makes it excellent for querying and
-- wrong for this column, whose entire job is to return the *exact* bytes the
-- original request received. A replayed response that differs byte-for-byte
-- from the original is a weaker guarantee than the API promises, and it changes
-- Content-Length between the two.
--
-- The json type validates the value is well-formed JSON but preserves the
-- original text, which is what this column needs.
ALTER TABLE idempotency_keys
    ALTER COLUMN response_body TYPE json USING response_body::text::json;
