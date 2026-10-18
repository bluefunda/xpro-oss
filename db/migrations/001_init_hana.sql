-- Example HANA target table matching the mapping in config.yaml. Adjust
-- columns to match your own `mapping` list — xpro-oss has no fixed schema.
-- HANA takes one statement per request; cmd/migrate splits on ';'.
CREATE TABLE records (
    account_number NVARCHAR(255),
    amount         NVARCHAR(255),
    raw_xml        NCLOB,
    inserted_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
