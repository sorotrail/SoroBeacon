package graphql

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type graphQLResponseEnvelope struct {
	Data   map[string]any `json:"data,omitempty"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions,omitempty"`
	} `json:"errors,omitempty"`
}

func TestNormalizeAndHashDocument(t *testing.T) {
	doc1 := `
		query GetStats {
			# comment here
			stats {
				monitors
				alerts
			}
		}
	`
	doc2 := "query GetStats {\n  stats {\n    monitors\n    alerts\n  }\n}"
	doc3 := "query GetStats{stats{monitors alerts}}"

	norm1, err := NormalizeDocument(doc1)
	require.NoError(t, err)
	norm2, err := NormalizeDocument(doc2)
	require.NoError(t, err)
	norm3, err := NormalizeDocument(doc3)
	require.NoError(t, err)

	assert.Equal(t, norm1, norm2)
	assert.Equal(t, norm1, norm3)

	hash1 := HashDocument(norm1)
	hash2 := HashDocument(norm2)
	hash3 := HashDocument(norm3)

	assert.Equal(t, hash1, hash2)
	assert.Equal(t, hash1, hash3)
	assert.Len(t, hash1, 64)

	// ValidateAndHashDocument
	norm, hash, err := ValidateAndHashDocument(doc1, hash1)
	require.NoError(t, err)
	assert.Equal(t, hash1, hash)
	assert.Equal(t, norm1, norm)

	// Mismatched expected hash
	_, _, err = ValidateAndHashDocument(doc1, "wronghash00000000000000000000000000000000000000000000000000000000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "operation hash mismatch")
}

func TestPersistedOperationUnknownIdentifier(t *testing.T) {
	_, st := newTestResolver(t)
	r := NewResolver(st, discardLogger())
	h := NewHandler(r, DefaultServerConfig())

	// Unknown ID via JSON id field
	reqBody := `{"id":"unknown-hash-12345"}`
	rec := postGraphQL(t, h, reqBody)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, "PersistedQueryNotFound", resp.Errors[0].Message)
	assert.Equal(t, CodePersistedQueryNotFound, resp.Errors[0].Extensions["code"])

	// Unknown ID via extensions.persistedQuery
	apqBody := `{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"apq-unknown-hash"}}}`
	rec = postGraphQL(t, h, apqBody)
	assert.Equal(t, http.StatusOK, rec.Code)

	var apqResp graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apqResp))
	require.Len(t, apqResp.Errors, 1)
	assert.Equal(t, CodePersistedQueryNotFound, apqResp.Errors[0].Extensions["code"])
}

func TestPersistedOperationInvocation(t *testing.T) {
	ctx := context.Background()
	_, st := newTestResolver(t)
	r := NewResolver(st, discardLogger())
	h := NewHandler(r, DefaultServerConfig())

	doc := `query DashboardStats { stats { monitors alerts } }`
	norm, hash, err := ValidateAndHashDocument(doc, "")
	require.NoError(t, err)

	err = st.RegisterPersistedOperation(ctx, store.PersistedOperation{
		ID:        hash,
		Name:      "DashboardStats",
		Document:  norm,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Invoke by id
	rec := postGraphQL(t, h, `{"id":"`+hash+`"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Errors)
	assert.NotNil(t, resp.Data)

	// Invoke by APQ extensions
	rec = postGraphQL(t, h, `{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"`+hash+`"}}}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var apqResp graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apqResp))
	assert.Empty(t, apqResp.Errors)
	assert.NotNil(t, apqResp.Data)
}

func TestAllowlistModeRejectsAdHocQuery(t *testing.T) {
	ctx := context.Background()
	_, st := newTestResolver(t)
	r := NewResolver(st, discardLogger())
	cfg := DefaultServerConfig()
	cfg.AllowlistOnly = true
	h := NewHandler(r, cfg)

	// 1. Ad-hoc query not registered -> refused
	adhoc := `{"query":"{ monitors { edges { node { id } } } }"}`
	rec := postGraphQL(t, h, adhoc)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Errors, 1)
	assert.Contains(t, resp.Errors[0].Message, "persisted operations only")
	assert.Equal(t, CodePersistedQueryNotAllowed, resp.Errors[0].Extensions["code"])

	// 2. Register the operation
	doc := `{ monitors { edges { node { id } } } }`
	norm, hash, err := ValidateAndHashDocument(doc, "")
	require.NoError(t, err)

	err = st.RegisterPersistedOperation(ctx, store.PersistedOperation{
		ID:        hash,
		Name:      "MonitorsList",
		Document:  norm,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// 3. Invoking registered query by id in allowlist mode -> succeeds
	rec = postGraphQL(t, h, `{"id":"`+hash+`"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var allowResp graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &allowResp))
	assert.Empty(t, allowResp.Errors)

	// 4. Sending the registered query document text in allowlist mode matches the allowlist hash and succeeds
	rec = postGraphQL(t, h, adhoc)
	assert.Equal(t, http.StatusOK, rec.Code)
	var textResp graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &textResp))
	assert.Empty(t, textResp.Errors)
}

func TestCostAndComplexityLimitsEnforcedOnPersistedOperations(t *testing.T) {
	ctx := context.Background()
	_, st := newTestResolver(t)
	r := NewResolver(st, discardLogger())
	cfg := ServerConfig{
		AllowlistOnly: true,
		MaxDepth:      2,
		MaxComplexity: 15,
	}
	h := NewHandler(r, cfg)

	// Deep query registered
	deepDoc := `query Deep { a { b { c { d { id } } } } }`
	normDeep, hashDeep, err := ValidateAndHashDocument(deepDoc, "")
	require.NoError(t, err)

	require.NoError(t, st.RegisterPersistedOperation(ctx, store.PersistedOperation{
		ID:        hashDeep,
		Name:      "DeepQuery",
		Document:  normDeep,
		CreatedAt: time.Now().UTC(),
	}))

	// Invoking deep query fails MaxDepth limit (depth 5 > 2)
	rec := postGraphQL(t, h, `{"id":"`+hashDeep+`"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Errors, 1)
	assert.Contains(t, resp.Errors[0].Message, "query depth")
	assert.Equal(t, CodeQueryDepthExceeded, resp.Errors[0].Extensions["code"])

	// High complexity query with variables (tested on handler with MaxDepth: 10, MaxComplexity: 15)
	hComplexity := NewHandler(r, ServerConfig{
		AllowlistOnly: true,
		MaxDepth:      10,
		MaxComplexity: 15,
	})

	listDoc := `query ListItems($limit: Int) { items(limit: $limit) { name value tag details { note } } }`
	normList, hashList, err := ValidateAndHashDocument(listDoc, "")
	require.NoError(t, err)

	require.NoError(t, st.RegisterPersistedOperation(ctx, store.PersistedOperation{
		ID:        hashList,
		Name:      "ListItems",
		Document:  normList,
		CreatedAt: time.Now().UTC(),
	}))

	// With limit 1 (complexity fits within 15) -> succeeds
	recLow := postGraphQL(t, hComplexity, `{"id":"`+hashList+`","variables":{"limit":1}}`)
	assert.Equal(t, http.StatusOK, recLow.Code)
	var respLow graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(recLow.Body.Bytes(), &respLow))
	assert.Empty(t, respLow.Errors)

	// With limit 50 (complexity exceeds 15) -> rejected
	recHigh := postGraphQL(t, hComplexity, `{"id":"`+hashList+`","variables":{"limit":50}}`)
	assert.Equal(t, http.StatusOK, recHigh.Code)
	var respHigh graphQLResponseEnvelope
	require.NoError(t, json.Unmarshal(recHigh.Body.Bytes(), &respHigh))
	require.Len(t, respHigh.Errors, 1)
	assert.Contains(t, respHigh.Errors[0].Message, "query complexity")
	assert.Equal(t, CodeQueryComplexityExceeded, respHigh.Errors[0].Extensions["code"])
}

func TestDefaultPersistedOperationsValid(t *testing.T) {
	ops := DefaultPersistedOperations()
	assert.NotEmpty(t, ops)

	for _, op := range ops {
		assert.NotEmpty(t, op.ID)
		assert.NotEmpty(t, op.Name)
		assert.NotEmpty(t, op.Document)
		assert.Equal(t, HashDocument(op.Document), op.ID)
	}
}
