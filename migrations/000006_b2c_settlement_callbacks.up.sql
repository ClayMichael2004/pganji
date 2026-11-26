ALTER TABLE external_clearing_records
    ADD COLUMN IF NOT EXISTS status VARCHAR(20) NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'SETTLED', 'FAILED', 'TIMED_OUT')),
    ADD COLUMN IF NOT EXISTS provider_receipt VARCHAR(128),
    ADD COLUMN IF NOT EXISTS result_code INT,
    ADD COLUMN IF NOT EXISTS result_desc TEXT,
    ADD COLUMN IF NOT EXISTS settled_at TIMESTAMPTZ;

-- Backfill existing C2B deposits as SETTLED since deposits only record upon M-Pesa confirmation
UPDATE external_clearing_records
SET status = 'SETTLED',
    settled_at = created_at
WHERE direction = 'DEPOSIT' AND status = 'PENDING';
