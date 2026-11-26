ALTER TABLE transactions
    ADD COLUMN IF NOT EXISTS reversed_transaction_id UUID REFERENCES transactions(id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS reversal_reason TEXT;

-- Guarantee at the database layer that a transaction can only be reversed once
CREATE UNIQUE INDEX IF NOT EXISTS idx_transactions_unique_reversal
ON transactions (reversed_transaction_id)
WHERE reversed_transaction_id IS NOT NULL;
