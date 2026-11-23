package poller

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCursorRoundTrip asserts that encodeCursor and decodeCursor preserve
// arbitrary batch indices and RPC continuation cursors across round-trips.
// A cursor that mutates between batches causes the poller to either re-ingest
// ledgers or skip them silently.
func TestCursorRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		batch     int
		rpcCursor string
	}{
		{
			name:      "zero batch and empty cursor",
			batch:     0,
			rpcCursor: "",
		},
		{
			name:      "zero batch with opaque cursor",
			batch:     0,
			rpcCursor: "cursor-12345",
		},
		{
			name:      "positive batch and standard cursor",
			batch:     1,
			rpcCursor: "0000000000000001-00000001",
		},
		{
			name:      "higher batch index",
			batch:     42,
			rpcCursor: "page_token_99",
		},
		{
			name:      "large batch index",
			batch:     10000,
			rpcCursor: "ledger-999999-entry-0",
		},
		{
			// The separator character '|' is the delimiter between batch and
			// rpcCursor. When the RPC's own cursor contains '|', strings.Cut
			// splits on the first occurrence so the remainder is preserved.
			name:      "rpc cursor containing separator",
			batch:     3,
			rpcCursor: "part1|part2|part3",
		},
		{
			name:      "rpc cursor with multiple consecutive separators",
			batch:     5,
			rpcCursor: "||leading-and-trailing||",
		},
		{
			name:      "rpc cursor with special characters and spaces",
			batch:     7,
			rpcCursor: "urn:stellar:event?id=123&type=contract#xyz / test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := encodeCursor(tt.batch, tt.rpcCursor)
			assert.NotEmpty(t, encoded)

			gotBatch, gotCursor := decodeCursor(encoded)
			assert.Equal(t, tt.batch, gotBatch, "batch mismatch")
			assert.Equal(t, tt.rpcCursor, gotCursor, "rpcCursor mismatch")
		})
	}
}

// TestCursorRoundTripProperty tests a continuous range of batch numbers
// and synthetic cursor values to verify the encoding invariant holds across
// multiple iterations.
func TestCursorRoundTripProperty(t *testing.T) {
	for batch := 0; batch <= 50; batch++ {
		cursor := fmt.Sprintf("rpc-cursor-%d-%x", batch, batch*7919)
		gotBatch, gotCursor := decodeCursor(encodeCursor(batch, cursor))
		assert.Equal(t, batch, gotBatch)
		assert.Equal(t, cursor, gotCursor)
	}
}

// TestDecodeCursorMalformed asserts that invalid or malformed cursor strings
// do not panic and safely fall back to the documented start-of-cycle default (0, "").
func TestDecodeCursorMalformed(t *testing.T) {
	tests := []struct {
		name   string
		cursor string
	}{
		{
			name:   "empty string",
			cursor: "",
		},
		{
			name:   "invalid base64 non-ascii",
			cursor: "not-valid-base64!@#$%",
		},
		{
			name:   "whitespace only",
			cursor: "   ",
		},
		{
			// Valid base64 encoding of a string that lacks the '|' separator.
			name:   "valid base64 without separator",
			cursor: base64.RawURLEncoding.EncodeToString([]byte("123456")),
		},
		{
			// Valid base64 encoding where the batch segment is non-numeric.
			name:   "valid base64 with non-numeric batch",
			cursor: base64.RawURLEncoding.EncodeToString([]byte("first|cursor-abc")),
		},
		{
			// Valid base64 encoding where the batch segment is empty.
			name:   "valid base64 with empty batch",
			cursor: base64.RawURLEncoding.EncodeToString([]byte("|cursor-abc")),
		},
		{
			// Arbitrary corrupt binary data.
			name:   "corrupt binary base64",
			cursor: base64.RawURLEncoding.EncodeToString([]byte{0x00, 0xff, 0xfe, 0x01}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotPanics(t, func() {
				batch, rpcCursor := decodeCursor(tt.cursor)
				assert.Equal(t, 0, batch, "expected default batch 0 on malformed input")
				assert.Equal(t, "", rpcCursor, "expected empty cursor on malformed input")
			})
		})
	}
}
