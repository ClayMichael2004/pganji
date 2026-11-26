package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// 1. Cross-Currency FX Volume & Execution Counters
	FXConversionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "pganji",
			Subsystem: "fx",
			Name:      "conversions_total",
			Help:      "Total number of completed cross-currency FX conversions by pair.",
		},
		[]string{"source_currency", "target_currency"},
	)

	FXVolumeMinorUnitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "pganji",
			Subsystem: "fx",
			Name:      "volume_minor_units_total",
			Help:      "Cumulative FX volume in minor units (cents) split by leg direction (source|target) and currency.",
		},
		[]string{"leg", "currency"},
	)

	// 2. External Clearing & B2C Callback Outcome Counters
	ClearingOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "pganji",
			Subsystem: "clearing",
			Name:      "operations_total",
			Help:      "Total external clearing operations by provider, direction (DEPOSIT|WITHDRAWAL), and terminal outcome (SETTLED|FAILED|TIMED_OUT|INITIATED).",
		},
		[]string{"provider", "direction", "status"},
	)

	ClearingVolumeMinorUnitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "pganji",
			Subsystem: "clearing",
			Name:      "volume_minor_units_total",
			Help:      "Cumulative clearing volume in minor units (cents) by direction and status.",
		},
		[]string{"direction", "status"},
	)

	// 3. Append-Only Compensating Reversal Counters
	ReversalsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "pganji",
			Subsystem: "ledger",
			Name:      "reversals_total",
			Help:      "Total number of compensating reversals posted by trigger source (MANUAL_API|B2C_AUTO_REVERSAL).",
		},
		[]string{"source"},
	)
)

// LedgerStateCollector computes point-in-time reconciliation and hash-chain gauges on every Prometheus scrape.
type LedgerStateCollector struct {
	pool                   *pgxpool.Pool
	chainHeightDesc        *prometheus.Desc
	trialBalanceNetDesc    *prometheus.Desc
	clearingDiscrepancyDesc *prometheus.Desc
	outboxBacklogDesc      *prometheus.Desc
	fxBridgeBalanceDesc    *prometheus.Desc
}

func NewLedgerStateCollector(pool *pgxpool.Pool) *LedgerStateCollector {
	return &LedgerStateCollector{
		pool: pool,
		chainHeightDesc: prometheus.NewDesc(
			"pganji_ledger_hash_chain_height",
			"Current monotonic sequence height (MAX(seq)) of the SHA-256 journal hash chain.",
			nil, nil,
		),
		trialBalanceNetDesc: prometheus.NewDesc(
			"pganji_reconciliation_trial_balance_net_minor_units",
			"Net trial balance discrepancy (SUM(DEBIT) - SUM(CREDIT)) per currency; 0 indicates a balanced ledger.",
			[]string{"currency"}, nil,
		),
		clearingDiscrepancyDesc: prometheus.NewDesc(
			"pganji_reconciliation_clearing_discrepancy_minor_units",
			"Difference between CLEARING-MPESA-KES ledger balance and net settled external clearing records; 0 indicates reconciled.",
			nil, nil,
		),
		outboxBacklogDesc: prometheus.NewDesc(
			"pganji_outbox_events_count",
			"Current count of transactional outbox events by delivery status.",
			[]string{"status"}, nil,
		),
		fxBridgeBalanceDesc: prometheus.NewDesc(
			"pganji_fx_bridge_net_balance_minor_units",
			"Net liquidity pool position of institutional FX bridge accounts in minor units.",
			[]string{"account_number", "currency"}, nil,
		),
	}
}

func (c *LedgerStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.chainHeightDesc
	ch <- c.trialBalanceNetDesc
	ch <- c.clearingDiscrepancyDesc
	ch <- c.outboxBacklogDesc
	ch <- c.fxBridgeBalanceDesc
}

func (c *LedgerStateCollector) Collect(ch chan<- prometheus.Metric) {
	if c.pool == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1. Journal Hash Chain Height
	var maxSeq int64
	if err := c.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM journal_entries`).Scan(&maxSeq); err == nil {
		ch <- prometheus.MustNewConstMetric(c.chainHeightDesc, prometheus.GaugeValue, float64(maxSeq))
	}

	// 2. Per-Currency Trial Balance Net
	tbRows, err := c.pool.Query(ctx, `
		SELECT a.currency,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE -j.amount END), 0)
		FROM accounts a
		LEFT JOIN journal_entries j ON a.id = j.account_id
		GROUP BY a.currency
	`)
	if err == nil {
		for tbRows.Next() {
			var curr string
			var net int64
			if scanErr := tbRows.Scan(&curr, &net); scanErr == nil {
				ch <- prometheus.MustNewConstMetric(c.trialBalanceNetDesc, prometheus.GaugeValue, float64(net), curr)
			}
		}
		tbRows.Close()
	}

	// 3. M-Pesa Clearing Reconciliation Discrepancy
	var actualClearing, expectedClearing int64
	_ = c.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN entry_type = 'DEBIT' THEN amount ELSE -amount END), 0)
		FROM journal_entries
		WHERE account_id = '00000000-0000-0000-0000-000000000001'
	`).Scan(&actualClearing)

	_ = c.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(
			CASE
				WHEN direction = 'WITHDRAWAL' AND status IN ('SETTLED', 'PENDING') THEN amount
				WHEN direction = 'DEPOSIT' AND status = 'SETTLED' THEN -amount
				ELSE 0
			END
		), 0)
		FROM external_clearing_records
	`).Scan(&expectedClearing)

	ch <- prometheus.MustNewConstMetric(
		c.clearingDiscrepancyDesc,
		prometheus.GaugeValue,
		float64(actualClearing-expectedClearing),
	)

	// 4. FX Bridge Pool Positions
	bridgeRows, err := c.pool.Query(ctx, `
		SELECT a.account_number, a.currency,
		       COALESCE(SUM(CASE WHEN j.entry_type = 'DEBIT' THEN j.amount ELSE -j.amount END), 0)
		FROM accounts a
		LEFT JOIN journal_entries j ON a.id = j.account_id
		WHERE a.id IN ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000003')
		GROUP BY a.account_number, a.currency
	`)
	if err == nil {
		for bridgeRows.Next() {
			var accNum, curr string
			var net int64
			if scanErr := bridgeRows.Scan(&accNum, &curr, &net); scanErr == nil {
				ch <- prometheus.MustNewConstMetric(c.fxBridgeBalanceDesc, prometheus.GaugeValue, float64(net), accNum, curr)
			}
		}
		bridgeRows.Close()
	}

	// 5. Outbox Delivery Counts by Status
	obRows, err := c.pool.Query(ctx, `SELECT status, COUNT(*) FROM outbox_events GROUP BY status`)
	if err == nil {
		for obRows.Next() {
			var status string
			var count int64
			if scanErr := obRows.Scan(&status, &count); scanErr == nil {
				ch <- prometheus.MustNewConstMetric(c.outboxBacklogDesc, prometheus.GaugeValue, float64(count), status)
			}
		}
		obRows.Close()
	}
}

// RegisterEnterpriseMetrics registers all FX, clearing, reversal, and reconciliation collectors once at startup.
func RegisterEnterpriseMetrics(pool *pgxpool.Pool) {
	prometheus.MustRegister(
		FXConversionsTotal,
		FXVolumeMinorUnitsTotal,
		ClearingOperationsTotal,
		ClearingVolumeMinorUnitsTotal,
		ReversalsTotal,
		NewLedgerStateCollector(pool),
	)
}
