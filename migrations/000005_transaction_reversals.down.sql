DROP INDEX IF EXISTS idx_transactions_unique_reversal;
ALTER TABLE transactions
    DROP COLUMN IF EXISTS reversal_reason,
    DROP COLUMN IF EXISTS reversed_transaction_id;
