package poller

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type upstreamAlertInspector interface {
	ListAlerts(ctx context.Context, f store.AlertFilter) ([]store.Alert, error)
}

type upstreamDeliveryInspector interface {
	ListDeliveryAttempts(ctx context.Context, alertID int64, status string) ([]store.DeliveryAttempt, error)
}

type upstreamEscalationCompleter interface {
	CompleteEscalation(ctx context.Context, alertID int64) error
}

// recordUpstreamMetrics updates upstream lag and failure gauges when the
// configured source implements UpstreamStatusSource, and returns the upstream's
// known chain head (0 when unavailable).
func (p *Poller) recordUpstreamMetrics() uint32 {
	u, ok := p.source.(UpstreamStatusSource)
	if !ok {
		return 0
	}
	chainHead, upstreamLag, failures := u.UpstreamStatus()
	p.metrics.SetUpstreamLag(int64(upstreamLag))
	p.metrics.SetUpstreamFailures(failures)
	return chainHead
}

// reconcileUpstreamRetractions checks whether the upstream indexer reported any
// retracted ledgers or event IDs during the poll cycle, marks affected alerts
// as retracted in SoroBeacon's store, rewinds checkpoints so replacement chain
// events are ingested, and emits a consumer-visible retraction notice for any
// alert that was already delivered.
func (p *Poller) reconcileUpstreamRetractions(ctx context.Context, state *store.IngestState, monitors []store.Monitor) error {
	u, ok := p.source.(UpstreamStatusSource)
	if !ok {
		return nil
	}
	retractedFrom, retractedIDs := u.ConsumeRetractions()
	if retractedFrom == 0 && len(retractedIDs) == 0 {
		return nil
	}

	retractedIDSet := make(map[string]bool, len(retractedIDs))
	for _, id := range retractedIDs {
		if id != "" {
			retractedIDSet[id] = true
		}
	}

	monitorByID := make(map[int64]store.Monitor, len(monitors))
	for _, m := range monitors {
		monitorByID[m.ID] = m
	}

	var affected []store.Alert
	if inspector, ok := p.store.(upstreamAlertInspector); ok {
		existing, err := inspector.ListAlerts(ctx, store.AlertFilter{
			Network: p.network,
			Limit:   500,
		})
		if err == nil {
			for _, a := range existing {
				if (retractedFrom > 0 && a.Ledger >= retractedFrom) || retractedIDSet[a.EventID] {
					affected = append(affected, a)
					if a.Ledger > 0 && (retractedFrom == 0 || a.Ledger < retractedFrom) {
						retractedFrom = a.Ledger
					}
				}
			}
		}
	}

	now := p.now().UTC()
	var retractedCount int64
	if retractedFrom > 0 {
		var err error
		retractedCount, err = p.store.RetractAlertsFromLedger(ctx, p.network, retractedFrom, now)
		if err != nil {
			return err
		}
		p.metrics.RecordReorg(retractedFrom)
		p.log.Warn("upstream ledger retraction detected",
			"from_ledger", retractedFrom,
			"retracted_alerts", retractedCount,
			"retracted_event_ids", len(retractedIDs))

		rewindTo := uint32(0)
		if retractedFrom > 1 {
			rewindTo = retractedFrom - 1
		}
		if state != nil && rewindTo < state.LastLedger {
			state.LastLedger = rewindTo
			state.LastCursor = ""
			if err := p.store.SetIngestState(ctx, p.network, *state); err != nil {
				return err
			}
		}
		if err := p.store.RewindContractCursors(ctx, p.network, rewindTo); err != nil {
			return err
		}
	}

	for _, a := range affected {
		if esc, ok := p.store.(upstreamEscalationCompleter); ok {
			_ = esc.CompleteEscalation(ctx, a.ID)
		}
		if !p.wasAlertDelivered(ctx, a.ID) {
			continue
		}
		p.emitRetractionNotice(ctx, a, monitorByID[a.MonitorID], now)
	}
	return nil
}

func (p *Poller) wasAlertDelivered(ctx context.Context, alertID int64) bool {
	if p.dispatch == nil {
		return false
	}
	inspector, ok := p.store.(upstreamDeliveryInspector)
	if !ok {
		return true
	}
	attempts, err := inspector.ListDeliveryAttempts(ctx, alertID, "")
	if err != nil {
		return true
	}
	if len(attempts) == 0 {
		// In-memory or fire-and-forget dispatchers do not write delivery_attempts
		// rows before returning; treat the alert as delivered so downstream
		// consumers receive the retraction.
		return true
	}
	for _, att := range attempts {
		if att.Status == "sent" || att.Status == "delivered" || att.Status == "ok" {
			return true
		}
	}
	return false
}

func (p *Poller) emitRetractionNotice(ctx context.Context, a store.Alert, m store.Monitor, retractedAt time.Time) {
	var body map[string]any
	_ = json.Unmarshal(a.Payload, &body)
	if body == nil {
		body = map[string]any{}
	}
	contractID, _ := body["contract_id"].(string)
	eventName, _ := body["event_name"].(string)
	txHash, _ := body["tx_hash"].(string)

	body["retracted"] = true
	body["retracted_at"] = retractedAt
	body["retracted_alert_id"] = a.ID
	body["retracted_event_id"] = a.EventID
	payload, err := json.Marshal(body)
	if err != nil {
		payload = a.Payload
	}

	retractEventName := "[RETRACTED]"
	if eventName != "" {
		retractEventName = "[RETRACTED] " + eventName
	}
	retractEventID := a.EventID + ":retracted"

	if p.live != nil {
		p.live.Publish(broadcast.Alert{
			ID:          a.ID,
			MonitorID:   a.MonitorID,
			MonitorName: m.Name,
			RuleID:      a.RuleID,
			EventID:     retractEventID,
			Payload:     payload,
			CreatedAt:   retractedAt,
		})
	}
	if p.dispatch != nil {
		p.dispatch.Dispatch(ctx, notify.Alert{
			ID:          a.ID,
			MonitorID:   a.MonitorID,
			MonitorName: m.Name,
			RuleID:      a.RuleID,
			RuleType:    "retraction",
			EventID:     retractEventID,
			ContractID:  contractID,
			EventName:   retractEventName,
			Ledger:      a.Ledger,
			TxHash:      txHash,
			Payload:     payload,
			CreatedAt:   retractedAt,
			Severity:    string(a.Severity),
		})
	}
}
