ALTER TABLE idempotency_keys
    ALTER COLUMN response_body TYPE jsonb USING response_body::text::jsonb;
