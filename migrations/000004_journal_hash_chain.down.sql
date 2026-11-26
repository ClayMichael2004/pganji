DROP TRIGGER IF EXISTS trg_journal_entries_immutable ON journal_entries;
DROP FUNCTION IF EXISTS prevent_journal_mutation();

DROP TRIGGER IF EXISTS trg_journal_entry_hash ON journal_entries;
DROP FUNCTION IF EXISTS compute_journal_entry_hash();

DROP INDEX IF EXISTS idx_journal_entries_entry_hash;
DROP INDEX IF EXISTS idx_journal_entries_seq;

ALTER TABLE journal_entries
    DROP COLUMN IF EXISTS entry_hash,
    DROP COLUMN IF EXISTS prev_hash,
    DROP COLUMN IF EXISTS seq;
