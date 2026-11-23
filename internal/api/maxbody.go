package api

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/metrics"
)

// decompressReadCloser bundles the decompressor reader with the underlying body
// and decompressor closer so both are closed when Close() is called.
type decompressReadCloser struct {
	io.Reader
	underlying   io.ReadCloser
	decompressor io.Closer
}

func (d *decompressReadCloser) Close() error {
	var err1 error
	if d.decompressor != nil {
		err1 = d.decompressor.Close()
	}
	err2 := d.underlying.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// MaxBodyMiddleware wraps POST/PATCH/PUT bodies with http.MaxBytesReader
// so an oversized write is truncated during the read rather than buffered
// first. GET/HEAD/OPTIONS are left alone so probes and list endpoints stay
// unaffected. When Content-Length is already above the limit the request
// is rejected with 413 before any read, through the same JSON envelope as
// other API errors — not a connection reset.
func MaxBodyMiddleware(limit int64) func(http.Handler) http.Handler {
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	return MaxBodyMiddlewareWithResolver(func(_ *http.Request) int64 { return limit }, nil)
}

// MaxBodyMiddlewareWithResolver enables per-request body limit resolution
// (such as bulk endpoint overrides) and metrics recording on rejection.
// When Content-Encoding is gzip or deflate, it decompresses the body on the fly
// while bounding the DECOMPRESSED size by the limit, preventing compression bombs.
func MaxBodyMiddlewareWithResolver(resolver func(r *http.Request) int64, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPatch, http.MethodPut:
				limit := DefaultMaxBodyBytes
				if resolver != nil {
					if n := resolver(r); n > 0 {
						limit = n
					}
				}

				// If declared Content-Length already exceeds the limit, reject immediately
				if r.ContentLength > limit {
					if m != nil {
						m.RecordBodyRejection(rejectionLimitType(r))
					}
					writeErr(w, r, http.StatusRequestEntityTooLarge, "request body too large")
					return
				}

				encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
				if encoding == "gzip" || encoding == "deflate" {
					var decomp io.Reader
					var closer io.Closer

					if encoding == "gzip" {
						gz, err := gzip.NewReader(r.Body)
						if err != nil {
							writeErr(w, r, http.StatusBadRequest, "invalid gzip body: "+err.Error())
							return
						}
						decomp = gz
						closer = gz
					} else {
						fl := flate.NewReader(r.Body)
						decomp = fl
						closer = fl
					}

					// Crucial anti-compression-bomb protection: MaxBytesReader wraps the
					// DECOMPRESSED stream. A small compressed payload expanding to > limit
					// is stopped as soon as the decompressed byte count passes limit.
					r.Body = http.MaxBytesReader(w, &decompressReadCloser{
						Reader:       decomp,
						underlying:   r.Body,
						decompressor: closer,
					}, limit)
					r.ContentLength = -1
				} else {
					r.Body = http.MaxBytesReader(w, r.Body, limit)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rejectionLimitType determines whether the rejection is for a bulk operation or standard endpoint.
func rejectionLimitType(r *http.Request) string {
	path := r.URL.Path
	if strings.HasSuffix(path, "/bulk") || strings.HasSuffix(path, "/import") {
		return "bulk_max_body"
	}
	return "max_body"
}
