-- System clearing pool account (user_id is NULL for institutional system accounts)
INSERT INTO accounts (id, user_id, account_number, currency, type)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    NULL,
    'CLEARING-MPESA-KES',
    'KES',
    'LIABILITY'
)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS external_clearing_records (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID NOT NULL REFERENCES transactions(id) ON DELETE RESTRICT,
    provider VARCHAR(32) NOT NULL,          -- e.g. 'MPESA_C2B', 'MPESA_B2C'
    external_reference VARCHAR(128) UNIQUE NOT NULL, -- e.g. M-Pesa Receipt / ConversationID
    phone_number VARCHAR(20) NOT NULL,
    direction VARCHAR(16) NOT NULL CHECK (direction IN ('DEPOSIT', 'WITHDRAWAL')),
    amount BIGINT NOT NULL CHECK (amount > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
