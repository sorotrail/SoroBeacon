package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// Truncation reasons reported alongside a boundary.
const (
	// truncRetention: the range reaches past the retention horizon and the
	// archive was not consulted, so alerts older than the boundary are absent.
	truncRetention = "retention_horizon"
	// truncArchiveOff: the caller asked for archived rows but this instance
	// has no archive configured, so the answer ends at the boundary.
	truncArchiveOff = "archive_not_enabled"
	// truncArchiveCap: the archive read hit the per-request fetch cap
	// (store.DefaultArchiveMaxChunks chunks) and stopped at the boundary.
	truncArchiveCap = "archive_fetch_cap"
)

// alertTruncation says whether an alert answer is shorter than the requested
// range and, if so, where it stops. Every alert surface (listing, CSV and
// NDJSON export) reports it the same way, so they cannot disagree.
type alertTruncation struct {
	Truncated bool
	// Boundary is the oldest instant the answer is complete back to (newest
	// instant, for ascending sort, when the archive cap stopped the read).
	Boundary time.Time
	Reason   string
	// capped marks that more archived rows remain, so a short page still has
	// a cursor worth following.
	capped bool
}

// parseIncludeArchived reads the opt-in include_archived flag. Reading the
// archive is far slower than a database query, so it is never the default.
func parseIncludeArchived(w http.ResponseWriter, r *http.Request) (bool, bool) {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("include_archived")))
	switch v {
	case "", "false", "0":
		return false, true
	case "true", "1":
		return true, true
	}
	writeErr(w, r, http.StatusBadRequest, "invalid include_archived (want true or false)")
	return false, false
}

// WithArchiveReader lets alert queries opt in to archived history. Nil (the
// default) leaves every query database-only and truncation-reported.
func (s *Server) WithArchiveReader(ar store.ArchiveReader) *Server {
	s.archiveReader = ar
	return s
}

// failArchive reports an archive failure. A missing or corrupt chunk is an
// error naming the chunk, never a short result.
func (s *Server) failArchive(w http.ResponseWriter, r *http.Request, err error) bool {
	var ce *store.ArchiveChunkError
	if errors.As(err, &ce) {
		s.log.Error("archive read failed", "key", ce.Key, "reason", ce.Reason)
		writeErr(w, r, http.StatusBadGateway, "archived alerts unavailable: chunk "+ce.Key+": "+ce.Reason)
		return true
	}
	return false
}

func setTruncationHeaders(w http.ResponseWriter, tr alertTruncation) {
	if !tr.Truncated {
		w.Header().Set("X-Alerts-Truncated", "false")
		return
	}
	w.Header().Set("X-Alerts-Truncated", "true")
	w.Header().Set("X-Alerts-Truncation-Boundary", tr.Boundary.UTC().Format(time.RFC3339))
	w.Header().Set("X-Alerts-Truncation-Reason", tr.Reason)
}

// queryAlerts is ListAlerts across both tiers. The database always serves
// everything at or after the retention horizon; the archive serves what is
// strictly before it, so the tiers are disjoint and paging across the
// boundary neither repeats nor skips. Sort order is global: newest first
// reads the database then the archive, oldest first the reverse, and the
// cursor (an alert id) works unchanged in either tier.
//
// budget is the remaining archive chunk fetches for the whole request; pass
// nil for a single-page call. It is decremented as chunks are read so a
// multi-page export shares one cap.
func (s *Server) queryAlerts(ctx context.Context, f store.AlertFilter, include bool, budget *int) ([]store.Alert, alertTruncation, error) {
	var tr alertTruncation
	boundary, _ := s.workspaceRetentionHorizon(ctx)
	if boundary == nil || (!f.From.IsZero() && !f.From.Before(*boundary)) {
		out, err := s.store.ListAlerts(ctx, f)
		return out, tr, err
	}
	b := boundary.UTC()
	if !include || s.archiveReader == nil {
		out, err := s.store.ListAlerts(ctx, f)
		if err == nil && !f.From.IsZero() {
			tr = alertTruncation{Truncated: true, Boundary: b, Reason: truncRetention}
			if include {
				tr.Reason = truncArchiveOff
			}
		}
		return out, tr, err
	}

	if budget == nil {
		n := store.DefaultArchiveMaxChunks
		budget = &n
	}
	ws, ok := workspace.From(ctx)
	if !ok || ws == "" {
		ws = workspace.Default
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	asc := f.Sort == "created_at_asc"

	// A cursor that the database does not know is an archived alert's id.
	cursorArchived := false
	if f.AfterID > 0 {
		if _, err := s.store.GetAlert(ctx, f.AfterID); errors.Is(err, store.ErrNotFound) {
			cursorArchived = true
		} else if err != nil {
			return nil, tr, err
		}
	}

	read := func(after int64, n int) (store.ArchiveResult, error) {
		if *budget <= 0 {
			return store.ArchiveResult{Capped: true}, nil
		}
		af := f
		af.AfterID, af.Limit = after, n
		if af.To.IsZero() || af.To.After(b) {
			af.To = b
		}
		res, err := s.archiveReader.ListArchivedAlerts(ctx, ws, store.ArchiveQuery{Filter: af, MaxChunks: *budget})
		*budget -= res.ChunksFetched
		return res, err
	}
	capped := func(rows []store.Alert) alertTruncation {
		edge := b
		if len(rows) > 0 {
			edge = rows[len(rows)-1].CreatedAt.UTC()
		}
		return alertTruncation{Truncated: true, Boundary: edge, Reason: truncArchiveCap, capped: true}
	}

	var out []store.Alert
	if !asc {
		if !cursorArchived {
			var err error
			if out, err = s.store.ListAlerts(ctx, f); err != nil {
				return nil, tr, err
			}
		}
		if len(out) >= limit {
			return out, tr, nil
		}
		after := int64(0)
		if cursorArchived {
			after = f.AfterID
		}
		res, err := read(after, limit-len(out))
		if err != nil {
			return nil, tr, err
		}
		out = append(out, res.Alerts...)
		if res.Capped {
			tr = capped(res.Alerts)
		}
		return out, tr, nil
	}

	// Oldest first: archive, then database.
	if f.AfterID > 0 && !cursorArchived {
		out, err := s.store.ListAlerts(ctx, f)
		return out, tr, err
	}
	after := int64(0)
	if cursorArchived {
		after = f.AfterID
	}
	res, err := read(after, limit)
	if err != nil {
		return nil, tr, err
	}
	out = res.Alerts
	if res.Capped {
		return out, capped(out), nil
	}
	if cursorArchived && !res.CursorFound {
		return out, tr, nil // unknown cursor: an empty page, not a restart
	}
	if len(out) < limit {
		db := f
		db.AfterID, db.Limit = 0, limit-len(out)
		rows, err := s.store.ListAlerts(ctx, db)
		if err != nil {
			return nil, tr, err
		}
		out = append(out, rows...)
	}
	return out, tr, nil
}

// horizonTruncation is the truncation known before any row is read: it is
// what exports announce in their headers, since a streamed body cannot
// change them afterwards.
func (s *Server) horizonTruncation(ctx context.Context, f store.AlertFilter, include bool) alertTruncation {
	boundary, _ := s.workspaceRetentionHorizon(ctx)
	if boundary == nil || f.From.IsZero() || !f.From.Before(*boundary) {
		return alertTruncation{}
	}
	tr := alertTruncation{Truncated: true, Boundary: boundary.UTC(), Reason: truncRetention}
	if include {
		tr.Reason = truncArchiveOff
	}
	return tr
}

// beginTruncationReport sets the truncation headers for a streamed export.
// When the archive is being read, whether the cap is hit is only known at the
// end, so the result is declared as HTTP trailers instead and the caller
// finishes with endTruncationReport.
func (s *Server) beginTruncationReport(ctx context.Context, w http.ResponseWriter, f store.AlertFilter, include bool) bool {
	if include && s.archiveReader != nil {
		w.Header().Set("Trailer", "X-Alerts-Truncated, X-Alerts-Truncation-Boundary, X-Alerts-Truncation-Reason")
		return true
	}
	setTruncationHeaders(w, s.horizonTruncation(ctx, f, include))
	return false
}

func endTruncationReport(w http.ResponseWriter, trailers bool, tr alertTruncation) {
	if trailers {
		setTruncationHeaders(w, tr)
	}
}

// exportAlertRows feeds fn every alert the filter matches, up to f.Limit. Without
// archive reads it is the store's own stream; with them it pages queryAlerts
// under one shared fetch cap and records any truncation in tr.
func (s *Server) exportAlertRows(ctx context.Context, f store.AlertFilter, include bool, tr *alertTruncation, fn func(store.Alert) error) error {
	if !include || s.archiveReader == nil {
		return s.store.ListAlertsStream(ctx, f, fn)
	}
	budget := store.DefaultArchiveMaxChunks
	remaining := f.Limit
	for remaining > 0 {
		f.Limit = min(remaining, alertExportPageSize)
		page, ptr, err := s.queryAlerts(ctx, f, true, &budget)
		if err != nil {
			return err
		}
		if ptr.Truncated {
			*tr = ptr
		}
		for _, a := range page {
			if err := fn(a); err != nil {
				return err
			}
		}
		if len(page) < f.Limit || len(page) == 0 {
			return nil
		}
		remaining -= len(page)
		f.AfterID = page[len(page)-1].ID
	}
	return nil
}
