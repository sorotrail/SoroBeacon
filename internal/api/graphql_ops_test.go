package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/api/graphql"
	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestPersistedOperationsAPI(t *testing.T) {
	router, _, _ := newTokenServer(t, nil)

	// 1. Unauthenticated request rejected with 401
	res := callJSON(t, router, http.MethodPost, "/graphql/operations", "", `{"document":"{ stats { monitors } }"}`)
	assert.Equal(t, http.StatusUnauthorized, res.Code, "registration must be privileged and authenticated")

	// 2. Missing document rejected
	res = callJSON(t, router, http.MethodPost, "/graphql/operations", acmeToken, `{}`)
	assert.Equal(t, http.StatusBadRequest, res.Code)

	// 3. Malformed GraphQL syntax rejected
	res = callJSON(t, router, http.MethodPost, "/graphql/operations", acmeToken, `{"document":"not valid graphql {"}`)
	assert.Equal(t, http.StatusBadRequest, res.Code)

	// 4. Valid registration
	doc := `query DashboardStats { stats { monitors alerts } }`
	res = callJSON(t, router, http.MethodPost, "/graphql/operations", acmeToken, `{"name":"DashboardStats","document":`+strconvQuote(doc)+`}`)
	assert.Equal(t, http.StatusCreated, res.Code)

	var created store.PersistedOperation
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &created))
	assert.Equal(t, "DashboardStats", created.Name)
	assert.NotEmpty(t, created.ID)
	expectedHash := graphql.HashDocument(created.Document)
	assert.Equal(t, expectedHash, created.ID)

	// 5. Registration with mismatched expected ID rejected
	res = callJSON(t, router, http.MethodPost, "/graphql/operations", acmeToken,
		`{"id":"wronghash00000000000000000000000000000000000000000000000000000000","document":`+strconvQuote(doc)+`}`)
	assert.Equal(t, http.StatusBadRequest, res.Code)

	// 6. Registration with matching expected ID accepted
	res = callJSON(t, router, http.MethodPost, "/graphql/operations", acmeToken,
		`{"id":"`+created.ID+`","document":`+strconvQuote(doc)+`}`)
	assert.Equal(t, http.StatusCreated, res.Code)

	// 7. Get by ID
	res = callJSON(t, router, http.MethodGet, "/graphql/operations/"+created.ID, acmeToken, "")
	assert.Equal(t, http.StatusOK, res.Code)
	var got store.PersistedOperation
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &got))
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, created.Document, got.Document)

	// 8. List operations (reviewable)
	res = callJSON(t, router, http.MethodGet, "/graphql/operations", acmeToken, "")
	assert.Equal(t, http.StatusOK, res.Code)
	var listResp struct {
		Operations []store.PersistedOperation `json:"operations"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &listResp))
	assert.Len(t, listResp.Operations, 1)
	assert.Equal(t, created.ID, listResp.Operations[0].ID)

	// 9. Delete operation
	res = callJSON(t, router, http.MethodDelete, "/graphql/operations/"+created.ID, acmeToken, "")
	assert.Equal(t, http.StatusNoContent, res.Code)

	// 10. Get after delete returns 404
	res = callJSON(t, router, http.MethodGet, "/graphql/operations/"+created.ID, acmeToken, "")
	assert.Equal(t, http.StatusNotFound, res.Code)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
