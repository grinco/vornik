package service

// Trading-broker reconciliation extracted from container.go as
// part of the 2026-05-16 service-package split. The reconciler
// runs once at daemon startup against the live IBKR broker and
// fixes audit rows that diverged from the broker's view during a
// crash / restart window. Best-effort: broker unreachable is a
// log-and-skip, not a startup failure.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// reconcileStaleTradingOrders runs once at daemon startup. It
// scans trading_orders for status='submitted' rows older than
// the staleness threshold, queries the broker for current
// status, and writes terminal-status updates back through the
// audit channel for any rows IBKR shows as filled / cancelled
// / rejected. Rows IBKR doesn't recognise at all are flipped to
// "orphaned" so the soak panel stops counting them as live.
//
// Why this exists: pre-fix a daemon restart while a cancel was
// in flight could leave the trading_orders row stuck in
// "submitted" forever — the cancel landed at IBKR (real-world
// state matches that) but the daemon's audit row never got the
// status update. The next startup catches and corrects.
//
// Best-effort. Broker-unreachable startup logs and skips; the
// next start gets another chance. We deliberately use a long
// staleness threshold (24h) so a healthy submitted-but-not-
// yet-filled order doesn't get reconciled prematurely on a
// quick restart.
func (c *Container) reconcileStaleTradingOrders(ctx context.Context) {
	const staleThreshold = 24 * time.Hour
	if c.DB == nil {
		return
	}
	repo := c.repos.TradingOrders
	since := time.Time{}
	until := time.Now().UTC().Add(-staleThreshold)
	submitted := "submitted"
	rows, err := repo.List(ctx, persistence.TradingOrderFilter{
		Status:   &submitted,
		Since:    &since,
		Until:    &until,
		PageSize: 500,
	})
	if err != nil {
		c.Logger.Warn().Err(err).Msg("trading reconcile: list stale rows failed")
		return
	}
	if len(rows) == 0 {
		c.Logger.Debug().Msg("trading reconcile: no stale submitted rows")
		return
	}
	c.Logger.Info().
		Int("stale_count", len(rows)).
		Dur("threshold", staleThreshold).
		Msg("trading reconcile: scanning stale submitted orders against broker")

	brokerURL := os.Getenv("VORNIK_BROKER_BASE_URL")
	if brokerURL == "" {
		brokerURL = "http://127.0.0.1:8788"
	}

	// Fetch the broker's current open + recent orders snapshot
	// once and index by clientTag. The broker's recent list
	// covers ~24h of terminal orders, so the staleness window
	// + the broker's retention align.
	brokerByTag, err := fetchBrokerOrdersByTag(ctx, brokerURL)
	if err != nil {
		c.Logger.Warn().Err(err).Str("broker_url", brokerURL).Msg("trading reconcile: broker get_orders failed; skipping")
		return
	}

	now := time.Now().UTC()
	flipped := 0
	for _, row := range rows {
		if row == nil {
			continue
		}
		brokerOrd, found := brokerByTag[row.ID]
		newStatus := ""
		reason := ""
		if !found {
			// Broker doesn't know this clientTag — too old to
			// be in the recent list, never landed at IBKR, or
			// purged. Mark orphaned so the ledger reflects
			// uncertainty rather than a fictional live row.
			newStatus = "orphaned"
			reason = "boot_reconcile: broker did not recognise clientTag"
		} else {
			switch brokerOrd.Status {
			case "filled":
				newStatus = "filled"
				reason = "boot_reconcile: broker reports filled"
			case "cancelled":
				newStatus = "cancelled"
				reason = "boot_reconcile: broker reports cancelled"
			case "rejected":
				newStatus = "rejected"
				reason = "boot_reconcile: broker reports rejected"
			default:
				// Still submitted / partial — leave it alone.
				// The poll loop will pick it up on next
				// transition.
				continue
			}
		}
		row.Status = newStatus
		row.LastStatusReason = reason
		row.TerminalAt = &now
		if err := repo.Record(ctx, row); err != nil {
			c.Logger.Warn().Err(err).Str("order_id", row.ID).Msg("trading reconcile: persist failed")
			continue
		}
		flipped++
	}
	c.Logger.Info().
		Int("scanned", len(rows)).
		Int("flipped", flipped).
		Msg("trading reconcile: complete")
}

// fetchBrokerOrdersByTag calls the broker's get_orders MCP
// tool and indexes the result by clientTag for O(1) lookup
// during reconciliation. JSON-RPC envelope shape mirrors what
// the strategist + executor see, so the same parsing applies.
func fetchBrokerOrdersByTag(ctx context.Context, brokerURL string) (map[string]reconcileBrokerOrder, error) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_orders",
			"arguments": map[string]any{},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, brokerURL+"/message", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("broker returned %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Result.Content) == 0 {
		return nil, fmt.Errorf("empty broker response")
	}
	var ordersResp struct {
		Open   []reconcileBrokerOrder `json:"open"`
		Recent []reconcileBrokerOrder `json:"recent"`
	}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &ordersResp); err != nil {
		return nil, err
	}
	out := make(map[string]reconcileBrokerOrder, len(ordersResp.Open)+len(ordersResp.Recent))
	for _, o := range ordersResp.Open {
		out[o.ClientTag] = o
	}
	for _, o := range ordersResp.Recent {
		out[o.ClientTag] = o
	}
	return out, nil
}

// reconcileBrokerOrder is the trimmed shape the boot
// reconciler reads back from the broker's get_orders tool. We
// only need clientTag + status; ignoring the rest of
// OrderResponse keeps the JSON unmarshal targeted.
type reconcileBrokerOrder struct {
	ClientTag string `json:"client_tag"`
	Status    string `json:"status"`
}
