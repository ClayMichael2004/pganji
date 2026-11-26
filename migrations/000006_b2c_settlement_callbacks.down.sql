ALTER TABLE external_clearing_records
    DROP COLUMN IF EXISTS settled_at,
    DROP COLUMN IF EXISTS result_desc,
    DROP COLUMN IF EXISTS result_code,
    DROP COLUMN IF EXISTS provider_receipt,
    DROP COLUMN IF EXISTS status;
