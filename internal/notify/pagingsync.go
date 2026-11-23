package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// Acknowledgement sync with PagerDuty.
//
// Two directions share one rule that makes loops structurally impossible: an
// update carries the origin it came from in its context. The inbound webhook
// stamps the context with originPagerDuty before it calls the store's
// AcknowledgeAlert, and Propagate refuses to send anything outward when the
// context carries an origin. There is no flag to forget to pass: the same
// context that reaches the store is the one Propagate inspects. The stored
// provider state is a second guard, so an update the provider already has is
// never sent again.
//
// Conflict rule: state only moves forward (open, acknowledged, resolved).
// The first acknowledgement keeps its actor and time, because the store's
// AcknowledgeAlert is idempotent. A resolution outranks an acknowledgement.
// A later or replayed event can never move an alert backwards, so arrival
// order does not decide the outcome. SoroBeacon has no persisted "resolved"
// alert state, so a provider resolution stops escalation exactly as an
// acknowledgement does and is recorded as resolved on the provider ref.

const (
	// ProviderPagerDuty names the provider on an AlertExternalRef.
	ProviderPagerDuty = "pagerduty"

	originPagerDuty = "pagerduty"

	pagingSyncTimeout = 10 * time.Second
	maxWebhookBody    = 1 << 20
)

type originKey struct{}

// WithOrigin marks ctx as carrying an update that came from an external
// system. Propagate never sends such an update back out.
func WithOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, originKey{}, origin)
}

// OriginFrom returns the external origin of ctx, or "".
func OriginFrom(ctx context.Context) string {
	s, _ := ctx.Value(originKey{}).(string)
	return s
}

func stateRank(s string) int {
	switch s {
	case store.ExternalStateResolved:
		return 2
	case store.ExternalStateAcknowledged:
		return 1
	}
	return 0
}

// PagingSync pushes local acknowledgements to PagerDuty and accepts the
// provider's. A nil *PagingSync is a no-op, so callers need no guard.
type PagingSync struct {
	store   store.Store
	factory *Factory
	log     *slog.Logger
}

// NewPagingSync builds the sync. A nil store returns nil.
func NewPagingSync(st store.Store, f *Factory, log *slog.Logger) *PagingSync {
	if st == nil || f == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &PagingSync{store: st, factory: f, log: log}
}

// Propagate sends a local "acknowledge" or "resolve" to the provider incident
// of every PagerDuty channel the alert was delivered to. The provider being
// unreachable never fails the local action: the error is recorded on the ref
// (visible beside the alert) and logged, and local state stays authoritative.
func (p *PagingSync) Propagate(ctx context.Context, alertID int64, action string) {
	if p == nil || OriginFrom(ctx) != "" {
		return
	}
	want := store.ExternalStateAcknowledged
	if action == "resolve" {
		want = store.ExternalStateResolved
	}
	refs, err := p.store.ListAlertExternalRefs(ctx, alertID)
	if err != nil {
		p.log.Warn("list paging refs", "alert_id", alertID, "err", err)
		return
	}
	for _, ref := range refs {
		if ref.Provider != ProviderPagerDuty || stateRank(ref.State) >= stateRank(want) {
			continue
		}
		p.propagateOne(ctx, ref, action, want)
	}
}

func (p *PagingSync) propagateOne(ctx context.Context, ref store.AlertExternalRef, action, want string) {
	ch, err := p.store.GetChannel(ctx, ref.ChannelID)
	if err != nil {
		return
	}
	n, err := p.factory.NewContext(ctx, ch.Type, ch.Config)
	pd, ok := n.(*PagerDuty)
	if err != nil || !ok {
		_ = p.store.UpdateAlertExternalRef(ctx, ref.AlertID, ref.ChannelID, "", ref.State, "channel configuration unusable")
		return
	}
	sctx, cancel := context.WithTimeout(ctx, pagingSyncTimeout)
	defer cancel()
	if err := pd.SendAction(sctx, ref.DedupKey, action); err != nil {
		reason := "provider unreachable"
		var se *StatusError
		if errors.As(err, &se) {
			reason = "provider returned status " + strconv.Itoa(se.Code)
		}
		p.log.Warn("paging update failed; local state stays authoritative", "alert_id", ref.AlertID, "channel_id", ref.ChannelID, "action", action, "reason", reason)
		_ = p.store.UpdateAlertExternalRef(ctx, ref.AlertID, ref.ChannelID, "", ref.State, reason)
		return
	}
	_ = p.store.UpdateAlertExternalRef(ctx, ref.AlertID, ref.ChannelID, "", want, "")
}

// RecordDelivery stores the dedup key a PagerDuty delivery used, so a later
// webhook or local acknowledgement can find the incident by column.
func (p *PagingSync) RecordDelivery(ctx context.Context, ch store.Channel, a Alert) {
	if p == nil || ch.Type != TypePagerDuty || a.ID == 0 {
		return
	}
	if err := p.store.RecordAlertExternalRef(ctx, &store.AlertExternalRef{
		AlertID: a.ID, ChannelID: ch.ID, Provider: ProviderPagerDuty, DedupKey: dedupKey(a),
	}); err != nil {
		p.log.Warn("record paging ref", "alert_id", a.ID, "channel_id", ch.ID, "err", err)
	}
}

// pagerDutyWebhook is the part of a PagerDuty v3 webhook payload this reads.
type pagerDutyWebhook struct {
	Event struct {
		EventType string `json:"event_type"`
		Agent     struct {
			Summary string `json:"summary"`
		} `json:"agent"`
		Data struct {
			ID          string `json:"id"`
			IncidentKey string `json:"incident_key"`
		} `json:"data"`
	} `json:"event"`
}

// VerifyPagerDutySignature checks the X-PagerDuty-Signature header
// ("v1=<hex>[,v1=<hex>...]", HMAC-SHA256 of the raw body) in constant time.
// Several candidates are sent during a PagerDuty secret rotation; any match
// passes. An empty secret never verifies, so an unconfigured channel refuses
// every inbound request rather than accepting them all.
func VerifyPagerDutySignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	matched := false
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		hexSig, ok := strings.CutPrefix(part, "v1=")
		if !ok {
			continue
		}
		got, err := hex.DecodeString(hexSig)
		if err != nil {
			continue
		}
		// Evaluate every candidate: no early return on the first match.
		if hmac.Equal(got, want) {
			matched = true
		}
	}
	return matched
}

// WebhookHandler serves POST {prefix}{workspace}/{channelID}, the URL to
// configure as a PagerDuty webhook subscription. It is deliberately not
// under /api/v1: PagerDuty cannot present a SoroBeacon token, so the HMAC
// signature is the whole of the authentication, and keeping it off the
// token-authenticated router means no role or scope table can be misread as
// covering it. Every failure before the signature checks out answers the same
// 401, so the endpoint does not reveal which workspaces or channels exist.
func (p *PagingSync) WebhookHandler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ws, err := workspace.Parse(parts[0])
		chID, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || err2 != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
		if err != nil || len(body) > maxWebhookBody {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ctx := workspace.With(r.Context(), ws)
		ch, err := p.store.GetChannel(ctx, chID)
		if err != nil || ch.Type != TypePagerDuty {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		n, err := p.factory.NewContext(ctx, ch.Type, ch.Config)
		pd, ok := n.(*PagerDuty)
		if err != nil || !ok || !VerifyPagerDutySignature(pd.cfg.WebhookSecret, body, r.Header.Get("X-PagerDuty-Signature")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		p.applyInbound(WithOrigin(ctx, originPagerDuty), ch.ID, body, w)
	})
}

func (p *PagingSync) applyInbound(ctx context.Context, channelID int64, body []byte, w http.ResponseWriter) {
	var wh pagerDutyWebhook
	if err := json.Unmarshal(body, &wh); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var state string
	switch wh.Event.EventType {
	case "incident.acknowledged":
		state = store.ExternalStateAcknowledged
	case "incident.resolved":
		state = store.ExternalStateResolved
	case "incident.triggered":
		state = store.ExternalStateOpen
	default:
		writeWebhookStatus(w, "ignored")
		return
	}
	ref, err := p.store.GetAlertExternalRefByDedupKey(ctx, channelID, ProviderPagerDuty, wh.Event.Data.IncidentKey)
	if err != nil {
		// Not one of ours (or not recorded yet). 200 so PagerDuty does not retry.
		writeWebhookStatus(w, "ignored")
		return
	}
	if state != store.ExternalStateOpen {
		// The store's own acknowledgement path: it stamps the alert and stops
		// escalation in one transaction, exactly as a local acknowledgement.
		if err := p.store.AcknowledgeAlert(ctx, ref.AlertID, webhookActor(wh.Event.Agent.Summary)); err != nil {
			p.log.Error("apply inbound paging update", "alert_id", ref.AlertID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if err := p.store.UpdateAlertExternalRef(ctx, ref.AlertID, ref.ChannelID, wh.Event.Data.ID, state, ""); err != nil {
		p.log.Warn("record paging state", "alert_id", ref.AlertID, "err", err)
	}
	// Propagate is deliberately not called here, and would refuse anyway: the
	// context carries the pagerduty origin.
	writeWebhookStatus(w, "applied")
}

func writeWebhookStatus(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":%q}`, status)
}

// webhookActor is the acknowledger recorded for an inbound update. It is
// provider-supplied text, so it is prefixed, stripped of control characters
// and bounded.
func webhookActor(summary string) string {
	var b strings.Builder
	for _, r := range summary {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	name := strings.TrimSpace(b.String())
	if name == "" {
		return "pagerduty"
	}
	return "pagerduty:" + name
}
