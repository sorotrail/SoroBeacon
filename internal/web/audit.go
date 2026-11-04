package web

import (
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// AuditWebSpec describes the administrative action and target entity type a
// dashboard write route records in the audit log.
type AuditWebSpec struct {
	Action     string
	TargetType string
}

// AuditWebRouteTable maps every administrative mutating dashboard route
// ("METHOD /pattern") to the audit action and target type it records.
var AuditWebRouteTable = auth.NewRouteTable(map[string]AuditWebSpec{
	// Monitors
	"POST /monitors":                    {Action: store.AuditActionCreate, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/toggle":        {Action: store.AuditActionUpdate, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/delete":        {Action: store.AuditActionDelete, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/soft-delete":   {Action: store.AuditActionDelete, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/restore":       {Action: store.AuditActionUpdate, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/duplicate":     {Action: store.AuditActionCreate, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/channels":      {Action: store.AuditActionUpdate, TargetType: store.AuditTargetMonitor},

	// Rules
	"POST /monitors/{id}/rules":                 {Action: store.AuditActionCreate, TargetType: store.AuditTargetRule},
	"POST /monitors/{id}/rules/{ruleID}/toggle": {Action: store.AuditActionUpdate, TargetType: store.AuditTargetRule},
	"POST /monitors/{id}/rules/{ruleID}/delete": {Action: store.AuditActionDelete, TargetType: store.AuditTargetRule},

	// Channels
	"POST /channels":                {Action: store.AuditActionCreate, TargetType: store.AuditTargetChannel},
	"POST /channels/{id}":           {Action: store.AuditActionUpdate, TargetType: store.AuditTargetChannel},
	"POST /channels/{id}/delete":    {Action: store.AuditActionDelete, TargetType: store.AuditTargetChannel},
	"POST /channels/{id}/toggle":    {Action: store.AuditActionUpdate, TargetType: store.AuditTargetChannel},
	"POST /channels/{id}/duplicate": {Action: store.AuditActionCreate, TargetType: store.AuditTargetChannel},

	// API tokens
	"POST /tokens":             {Action: store.AuditActionCreate, TargetType: store.AuditTargetToken},
	"POST /tokens/{id}/revoke": {Action: store.AuditActionDelete, TargetType: store.AuditTargetToken},

	// Alerts & Dead letters
	"POST /alerts/{id}/acknowledge":   {Action: store.AuditActionUpdate, TargetType: store.AuditTargetAlert},
	"POST /dead-letters/{id}/redrive": {Action: store.AuditActionRedrive, TargetType: store.AuditTargetDeadLetter},
	"POST /dead-letters/{id}/discard": {Action: store.AuditActionDiscard, TargetType: store.AuditTargetDeadLetter},
	"POST /dead-letters/bulk/redrive": {Action: store.AuditActionRedrive, TargetType: store.AuditTargetDeadLetter},
	"POST /dead-letters/bulk/discard": {Action: store.AuditActionDiscard, TargetType: store.AuditTargetDeadLetter},

	// Maintenance windows
	"POST /maintenance":             {Action: store.AuditActionCreate, TargetType: store.AuditTargetMaintenanceWindow},
	"POST /maintenance/{id}/delete": {Action: store.AuditActionDelete, TargetType: store.AuditTargetMaintenanceWindow},
})

// AuditExemptWebRoutes documents mutating HTTP routes on the dashboard router
// that are intentionally excluded from the administrative audit log.
var AuditExemptWebRoutes = map[string]string{
	"POST /login":                                    "Session sign-in; pinned by TestNonConfigActionsWriteNoAuditEntry.",
	"POST /logout":                                   "Session sign-out; pinned by TestNonConfigActionsWriteNoAuditEntry.",
	"POST /theme":                                    "Browser theme cookie preference; pinned by TestNonConfigActionsWriteNoAuditEntry.",
	"POST /timezone":                                 "Browser timezone cookie preference; pinned by TestNonConfigActionsWriteNoAuditEntry.",
	"POST /channels/{id}/test":                       "Operational test notification send; pinned by TestNonConfigActionsWriteNoAuditEntry.",
	"POST /alerts/{id}/deliveries/{channelID}/retry": "Operational alert delivery retry; pinned by TestRetryDeliveryRecordsNewAttemptAndCallsNotifier.",
	"POST /monitors/bulk":                            "Bulk UI enable/disable helper; pinned by TestBulkMonitorsAppliesKnownIDsAndRedirects.",
	"POST /channels/bulk":                            "Bulk UI enable/disable helper for channels.",
	"POST /monitors/import":                          "Batch contract CSV/JSON upload helper.",
	"POST /searches":                                 "Saved alert search UI bookmark.",
	"POST /searches/{id}/delete":                     "Saved alert search UI bookmark deletion.",
	"POST /searches/{id}/default":                    "Saved alert search UI default bookmark toggle.",
	"POST /searches/{id}/undefault":                  "Saved alert search UI default bookmark clear.",
}

// actor is who a dashboard mutation is recorded as. The dashboard session
// carries a role and a workspace; when an OIDC identity or named principal is
// present in context, its label is used, otherwise the request id — the same
// value the log line carries.
func actor(r *http.Request) string {
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && p.Name != "" {
		return p.Name
	}
	return reqid.From(r)
}

func webActorType(r *http.Request) string {
	if p, ok := auth.PrincipalFromContext(r.Context()); ok {
		return p.EffectiveActorType()
	}
	return store.AuditActorPerson
}

func webSourceIP(r *http.Request) string {
	if r == nil || strings.TrimSpace(r.RemoteAddr) == "" {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

// audit records one append-only entry for a succeeded dashboard mutation.
// fields is the set of submitted field *names*; values are never passed here,
// because a channel form carries a webhook URL or token and the log must not
// hold it.
func (s *Server) audit(r *http.Request, action, targetType string, targetID int64, fields ...string) {
	s.auditOutcome(r, action, targetType, targetID, store.AuditOutcomeSucceeded, http.StatusSeeOther, "", fields...)
}

// auditOutcome records one append-only entry for a dashboard mutation with an
// explicit outcome, HTTP status code, and optional failure reason.
func (s *Server) auditOutcome(r *http.Request, action, targetType string, targetID int64, outcome string, statusCode int, reason string, fields ...string) {
	if s.store == nil {
		return
	}
	diff := json.RawMessage(`{}`)
	if len(fields) > 0 {
		if b, err := json.Marshal(map[string]any{"fields": fields}); err == nil {
			diff = store.SanitizeAuditDiff(targetType, b)
		}
	}
	entry := &store.AuditEntry{
		Actor:      actor(r),
		ActorType:  webActorType(r),
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		SourceIP:   webSourceIP(r),
		Surface:    store.AuditSurfaceDashboard,
		Outcome:    outcome,
		StatusCode: statusCode,
		Reason:     reason,
		Diff:       diff,
	}
	defer func() {
		if rec := recover(); rec != nil {
			if s.log != nil {
				s.log.Error("audit write panicked", "panic", rec)
			}
		}
	}()
	if err := s.store.CreateAuditEntry(r.Context(), entry); err != nil {
		s.log.Error("write audit entry",
			"action", action, "target_type", targetType, "target_id", targetID, "err", err)
	}
}

// auditPageSize is how many entries one page of the audit page shows. Fixed
// rather than a query parameter: the page has one paging affordance (an Older
// link), and a size knob would have to travel with every cursor.
const auditPageSize = 50

// auditPage serves GET /audit: the append-only record of who changed what,
// newest first. It makes the same store call the API's listAudit makes, so
// the page and the API cannot drift into two different answers about the same
// rows — the API's job is the filtering and paging, and this handler only
// collects it.
//
// Admin only (the role table says so): the log names the credential behind
// every change, and the pages it links to — monitors, channels — are what
// someone would be reading it to answer questions about.
func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditFilter{Limit: auditPageSize}

	// An unknown entity type or cursor is dropped rather than rejected, the
	// way the alerts page treats an unknown severity: the page still lists
	// and the control shows what was actually applied. A target id that
	// cannot be read is different — it is an explicit filter that would
	// silently widen to the whole log — so it stops the listing and says so,
	// with the entered value kept in the box to be fixed in place.
	note := ""
	targetType := strings.TrimSpace(q.Get("target_type"))
	if targetType != "" {
		if store.ValidAuditTargetType(targetType) {
			f.TargetType = targetType
		} else {
			targetType = ""
		}
	}
	targetIDInput := strings.TrimSpace(q.Get("target_id"))
	if targetIDInput != "" {
		id, err := strconv.ParseInt(targetIDInput, 10, 64)
		if err != nil || id < 1 {
			note = "That id could not be read. Enter a positive number, or clear the field."
		} else {
			f.TargetID = id
		}
	}
	f.Actor = strings.TrimSpace(q.Get("actor"))
	if v := q.Get("cursor"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id >= 1 {
			f.AfterID = id
		} else if note == "" {
			note = "That cursor could not be read. Start the listing again from the first page."
		}
	}

	var entries []store.AuditEntry
	if note == "" {
		var err error
		entries, err = s.store.ListAuditEntries(r.Context(), f)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	data := map[string]any{
		"Title":      "Audit log",
		"Entries":    entries,
		"TargetType": targetType,
		"TargetID":   targetIDInput,
		"Actor":      f.Actor,
		"Note":       note,
		"HasFilters": targetType != "" || targetIDInput != "" || f.Actor != "",
	}
	if note == "" && len(entries) == f.Limit {
		last := entries[len(entries)-1]
		data["OlderHref"] = template.URL("/audit?" + auditFilterQuery(f) + "cursor=" + strconv.FormatInt(last.ID, 10))
	}
	s.render(w, r, "audit", data)
}

// auditFilterQuery is the filter prefix preserved on the Older paging link:
// every control that narrowed the first page has to narrow the next one, or
// paging quietly widens back to the whole log halfway through it.
func auditFilterQuery(f store.AuditFilter) string {
	v := url.Values{}
	if f.TargetType != "" {
		v.Set("target_type", f.TargetType)
	}
	if f.TargetID != 0 {
		v.Set("target_id", strconv.FormatInt(f.TargetID, 10))
	}
	if f.Actor != "" {
		v.Set("actor", f.Actor)
	}
	enc := v.Encode()
	if enc == "" {
		return ""
	}
	return enc + "&"
}

// auditFields renders the changed field names an audit diff recorded. The
// diff carries only names — values are dropped at write time because a
// channel config holds webhook URLs, bot tokens and SMTP credentials — so
// there is nothing here that can leak, and a diff that is not the expected
// shape renders as a dash rather than as raw JSON the page would then have
// to be trusted not to print.
func auditFields(diff json.RawMessage) string {
	var d struct {
		Fields []string `json:"fields"`
	}
	if len(diff) == 0 || json.Unmarshal(diff, &d) != nil {
		return "—"
	}
	if len(d.Fields) == 0 {
		return "—"
	}
	return strings.Join(d.Fields, ", ")
}
