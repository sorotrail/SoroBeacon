package web

import (
	"encoding/json"
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// audit records one append-only entry for a dashboard mutation. fields is the
// set of submitted field *names*; values are never passed here, because a
// channel form carries a webhook URL or token and the log must not hold it.
//
// A failed audit write is logged and never fails the request: the operator's
// change already happened and must not be rolled back over a bookkeeping
// error.
func (s *Server) audit(r *http.Request, action, targetType string, targetID int64, fields ...string) {
	diff := json.RawMessage(`{}`)
	if len(fields) > 0 {
		if b, err := json.Marshal(map[string]any{"fields": fields}); err == nil {
			diff = b
		}
	}
	entry := &store.AuditEntry{
		Actor:      reqid.From(r),
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Diff:       diff,
	}
	if err := s.store.CreateAuditEntry(r.Context(), entry); err != nil {
		s.log.Error("write audit entry",
			"action", action, "target_type", targetType, "target_id", targetID, "err", err)
	}
}
