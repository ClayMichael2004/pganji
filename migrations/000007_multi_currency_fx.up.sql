-- 1. Provision institutional FX liquidity bridge accounts
INSERT INTO accounts (id, user_id, account_number, currency, type)
VALUES
    ('00000000-0000-0000-0000-000000000002', NULL, 'FX-BRIDGE-KES', 'KES', 'LIABILITY'),
    ('00000000-0000-0000-0000-000000000003', NULL, 'FX-BRIDGE-USD', 'USD', 'LIABILITY')
ON CONFLICT (id) DO NOTHING;

-- 2. Track cross-currency conversion executions & exchange rates
CREATE TABLE IF NOT EXISTS fx_conversions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID UNIQUE NOT NULL REFERENCES transactions(id) ON DELETE RESTRICT,
    from_account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    to_account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    source_currency VARCHAR(3) NOT NULL,
    target_currency VARCHAR(3) NOT NULL,
    source_amount BIGINT NOT NULL CHECK (source_amount > 0),
    target_amount BIGINT NOT NULL CHECK (target_amount > 0),
    exchange_rate NUMERIC(18, 8) NOT NULL CHECK (exchange_rate > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_fx_conversions_from_account ON fx_conversions (from_account_id);
CREATE INDEX IF NOT EXISTS idx_fx_conversions_to_account ON fx_conversions (to_account_id);
