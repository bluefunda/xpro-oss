-- Example target table matching the mapping in config.yaml. Adjust columns
-- to match your own `mapping` list — xpro-oss has no fixed schema.
CREATE TABLE IF NOT EXISTS public.records (
    account_number TEXT,
    amount          TEXT,
    raw_xml         TEXT,
    inserted_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
