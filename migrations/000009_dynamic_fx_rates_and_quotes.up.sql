-- 1. Real-time institutional FX market rates and buy/sell (bid/ask) spreads
CREATE TABLE IF NOT EXISTS fx_rates (
    pair VARCHAR(16) PRIMARY KEY, -- e.g. 'USD_KES', 'KES_USD'
    base_currency VARCHAR(3) NOT NULL,
    quote_currency VARCHAR(3) NOT NULL,
    mid_rate NUMERIC(18, 8) NOT NULL CHECK (mid_rate > 0),
    bid_rate NUMERIC(18, 8) NOT NULL CHECK (bid_rate > 0),
    ask_rate NUMERIC(18, 8) NOT NULL CHECK (ask_rate > 0),
    spread_bps INTEGER NOT NULL DEFAULT 100 CHECK (spread_bps >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Seed initial market rates for USD/KES and KES/USD
-- Base USD, Quote KES: Mid = 129.00 KES, Spread = 100 bps (1.00%)
-- Bid (Bank buys USD/sells KES): 129.00 * (1 - 0.01) = 127.71 KES
-- Ask (Bank sells USD/buys KES): 129.00 * (1 + 0.01) = 130.29 KES
INSERT INTO fx_rates (pair, base_currency, quote_currency, mid_rate, bid_rate, ask_rate, spread_bps)
VALUES 
    ('USD_KES', 'USD', 'KES', 129.00000000, 127.71000000, 130.29000000, 100),
    ('KES_USD', 'KES', 'USD', 0.00775194, 0.00767442, 0.00782946, 100)
ON CONFLICT (pair) DO NOTHING;

-- 2. Request-for-Quote (RFQ) guaranteed locked quotes with TTL
CREATE TABLE IF NOT EXISTS fx_quotes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_currency VARCHAR(3) NOT NULL,
    target_currency VARCHAR(3) NOT NULL,
    source_amount BIGINT NOT NULL CHECK (source_amount > 0),
    target_amount BIGINT NOT NULL CHECK (target_amount > 0),
    exchange_rate NUMERIC(18, 8) NOT NULL CHECK (exchange_rate > 0),
    rate_type VARCHAR(8) NOT NULL CHECK (rate_type IN ('BID', 'ASK', 'MID')),
    status VARCHAR(16) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'CONSUMED', 'EXPIRED')),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_fx_quotes_lookup ON fx_quotes (id, user_id, status, expires_at);
