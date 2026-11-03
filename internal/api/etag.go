package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// conditionalGET adds a strong ETag to a successful list response and answers
// a matching If-None-Match with 304 and no body.
//
// The tag is a hash of the serialised body. That is correct by construction:
// any field a client can see changes the bytes, so a bulk update or a toggle
// changes the tag with no per-endpoint bookkeeping. The cost is that the
// handler still runs its query and serialisation to find out the answer; only
// the transfer is saved. A cheaper key (max updated_at plus row count) would
// skip the query but misses changes the tables do not timestamp, such as
// bulk toggles and channel health.
//
// writeJSON sets Cache-Control: no-store, which forbids a client from keeping
// the response at all, so it would never have anything to revalidate. These
// responses use "private, no-cache" instead: a client may store the body but
// must revalidate before each reuse, and a shared proxy may not store it.
func conditionalGET(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		h := w.Header()
		for k, v := range rec.header {
			h[k] = v
		}
		if rec.status == http.StatusOK {
			sum := sha256.Sum256(rec.body.Bytes())
			etag := `"` + hex.EncodeToString(sum[:16]) + `"`
			h.Set("ETag", etag)
			h.Set("Cache-Control", "private, no-cache")
			if ETagMatches(r.Header.Get("If-None-Match"), etag) {
				h.Del("Content-Type")
				h.Del("Content-Length")
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

// ETagMatches reports whether an If-None-Match value names etag. "*" matches
// anything, and a weak validator (W/"x") compares equal to the strong "x".
func ETagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimPrefix(strings.TrimSpace(part), "W/")
		if part == "*" || part == etag {
			return true
		}
	}
	return false
}

// FormatETag formats a strong entity tag for a query. The tag incorporates every
// parameter that changes the answer: tenant workspace, route path, sorted query parameters,
// and the underlying backend change detector key.
func FormatETag(ws, path, query, changeKey string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s", ws, path, query, changeKey)))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func workspaceFromContext(ctx context.Context) string {
	if ws, ok := workspace.From(ctx); ok {
		return string(ws)
	}
	return string(workspace.Default)
}

// etagWriter wraps an http.ResponseWriter to stamp the ETag and Cache-Control
// headers when a successful (200 OK) response is written. It prevents downstream
// writeJSON helpers from overriding Cache-Control with no-store.
type etagWriter struct {
	http.ResponseWriter
	etag        string
	wroteHeader bool
}

func (w *etagWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if code == http.StatusOK {
		w.ResponseWriter.Header().Set("ETag", w.etag)
		w.ResponseWriter.Header().Set("Cache-Control", "private, no-cache")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *etagWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

type changeKeyFunc func(r *http.Request) (string, error)

// withConditionalETag wraps an endpoint handler with fast conditional request handling.
// It computes the strong entity tag from backend change detector state before running
// the handler's query. If If-None-Match matches, it writes HTTP 304 with no body and returns
// immediately without touching the expensive query. On non-match, it wraps ResponseWriter
// so that the generated ETag and Cache-Control: private, no-cache survive writeJSON.
func (s *Server) withConditionalETag(keyFn changeKeyFunc, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var changeKey string
		var err error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("panic in changeKey: %v", rec)
				}
			}()
			if keyFn != nil && s.store != nil {
				changeKey, err = keyFn(r)
			}
		}()
		if err != nil || changeKey == "" {
			// If change detector cannot evaluate (e.g. database degraded or channel missing or un-mocked),
			// proceed to next so the handler produces its natural error response.
			next(w, r)
			return
		}

		ws := workspaceFromContext(r.Context())
		etag := FormatETag(ws, r.URL.Path, r.URL.Query().Encode(), changeKey)

		if ETagMatches(r.Header.Get("If-None-Match"), etag) {
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "private, no-cache")
			w.Header().Del("Content-Type")
			w.Header().Del("Content-Length")
			w.WriteHeader(http.StatusNotModified)
			if s.metrics != nil {
				s.metrics.RecordNotModified(routePattern(r))
			}
			return
		}

		ew := &etagWriter{ResponseWriter: w, etag: etag}
		next(ew, r)
	}
}

func (s *Server) alertChangeKey(r *http.Request) (string, error) {
	if s.store == nil {
		return "", errors.New("no store")
	}
	maxID, count, err := s.store.AlertChangeKey(r.Context())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("alerts:%d:%d", maxID, count), nil
}

func (s *Server) alertsDailyChangeKey(r *http.Request) (string, error) {
	if s.store == nil {
		return "", errors.New("no store")
	}
	maxID, count, err := s.store.AlertChangeKey(r.Context())
	if err != nil {
		return "", err
	}
	// Incorporate current UTC calendar day so day boundary rollovers invalidate the tag
	// even if no new alerts arrived.
	today := time.Now().UTC().Format("2006-01-02")
	return fmt.Sprintf("daily:%s:%d:%d", today, maxID, count), nil
}

func (s *Server) channelStatsChangeKey(r *http.Request) (string, error) {
	if s.store == nil {
		return "", errors.New("no store")
	}
	id, err := pathID(r, "id")
	if err != nil {
		return "", err
	}
	maxID, count, err := s.store.ChannelDeliveryChangeKey(r.Context(), id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("channel:%d:delivery:%d:%d", id, maxID, count), nil
}

// bufferedResponse holds a handler's response so its body can be hashed
// before anything is sent.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
