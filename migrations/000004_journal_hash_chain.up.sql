-- 1. Add monotonic sequence and hash chain columns
ALTER TABLE journal_entries
    ADD COLUMN IF NOT EXISTS seq BIGSERIAL,
    ADD COLUMN IF NOT EXISTS prev_hash VARCHAR(64),
    ADD COLUMN IF NOT EXISTS entry_hash VARCHAR(64);

-- 2. Backfill existing historical rows deterministically in sequence order
DO $$
DECLARE
    r RECORD;
    v_prev VARCHAR(64) := repeat('0', 64);
    v_payload TEXT;
    v_hash VARCHAR(64);
BEGIN
    FOR r IN
        SELECT id, seq, transaction_id, account_id, entry_type, amount
        FROM journal_entries
        ORDER BY seq ASC
    LOOP
        v_payload := r.seq::text || '|' || v_prev || '|' || r.transaction_id::text || '|' ||
                     r.account_id::text || '|' || r.entry_type::text || '|' || r.amount::text;
        v_hash := encode(sha256(v_payload::bytea), 'hex');

        UPDATE journal_entries
        SET prev_hash = v_prev,
            entry_hash = v_hash
        WHERE id = r.id;

        v_prev := v_hash;
    END LOOP;
END $$;

ALTER TABLE journal_entries
    ALTER COLUMN prev_hash SET NOT NULL,
    ALTER COLUMN entry_hash SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_journal_entries_seq ON journal_entries (seq);
CREATE UNIQUE INDEX IF NOT EXISTS idx_journal_entries_entry_hash ON journal_entries (entry_hash);

-- 3. Trigger function to serialize tail linking and compute SHA-256 on insert
CREATE OR REPLACE FUNCTION compute_journal_entry_hash()
RETURNS TRIGGER AS $$
DECLARE
    v_prev_hash VARCHAR(64);
    v_payload TEXT;
BEGIN
    -- Serialize tail-pointer read across concurrent transactions (auto-released on COMMIT/ROLLBACK)
    PERFORM pg_advisory_xact_lock(9021042);

    SELECT entry_hash INTO v_prev_hash
    FROM journal_entries
    ORDER BY seq DESC
    LIMIT 1;

    IF v_prev_hash IS NULL THEN
        v_prev_hash := repeat('0', 64);
    END IF;

    NEW.prev_hash := v_prev_hash;
    v_payload := NEW.seq::text || '|' || NEW.prev_hash || '|' || NEW.transaction_id::text || '|' ||
                 NEW.account_id::text || '|' || NEW.entry_type::text || '|' || NEW.amount::text;
    NEW.entry_hash := encode(sha256(v_payload::bytea), 'hex');

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_journal_entry_hash ON journal_entries;
CREATE TRIGGER trg_journal_entry_hash
BEFORE INSERT ON journal_entries
FOR EACH ROW
EXECUTE FUNCTION compute_journal_entry_hash();

-- 4. Immutability guard blocking UPDATE or DELETE on posted journal entries
CREATE OR REPLACE FUNCTION prevent_journal_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger immutability violation: journal_entries are append-only (attempted %)', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_journal_entries_immutable ON journal_entries;
CREATE TRIGGER trg_journal_entries_immutable
BEFORE UPDATE OR DELETE ON journal_entries
FOR EACH ROW
EXECUTE FUNCTION prevent_journal_mutation();
