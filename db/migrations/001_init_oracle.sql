-- Example Oracle target table matching the mapping in config.yaml. Adjust
-- columns to match your own `mapping` list — xpro-oss has no fixed schema.
-- cmd/migrate splits on ';'. IF NOT EXISTS needs Oracle 23ai or later.
CREATE TABLE IF NOT EXISTS records (
    account_number VARCHAR2(255),
    amount         VARCHAR2(255),
    raw_xml        CLOB,
    inserted_at    TIMESTAMP DEFAULT SYSTIMESTAMP NOT NULL
);
