// Package api tests the rule endpoints.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/require"
)

// ruleStore is a test store that records calls and returns fixed data.
type ruleStore struct {
	store.Store
	createdRules   []*store.Rule
	updatedRules   []*store.Rule
	deletedRules   []int64
	listRules      []store.Rule
	getRule        *store.Rule
	getRuleErr     error
	getMonitorErr  error
	createErr      error
	updateErr      error
	deleteErr      error
}

func (s *ruleStore) CreateRule(ctx context.Context, r *store.Rule) error {
	if s.createErr != nil {
		return s.createErr
	}
	r.ID = int64(len(s.createdRules) + 1)
	s.createdRules = append(s.createdRules, r)
	return nil
}

func (s *ruleStore) CreateRules(ctx context.Context, rules []*store.Rule) error {
	for _, r := range rules {
		if err := s.CreateRule(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *ruleStore) GetRule(ctx context.Context, id int64) (*store.Rule, error) {
	if s.getRuleErr != nil {
		return nil, s.getRuleErr
	}
	if s.getRule != nil && s.getRule.ID == id {
		return s.getRule, nil
	}
	return nil, store.ErrNotFound
}

func (s *ruleStore) ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error) {
	return s.listRules, nil
}

func (s *ruleStore) UpdateRule(ctx context.Context, r *store.Rule) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updatedRules = append(s.updatedRules, r)
	return nil
}

func (s *ruleStore) DeleteRule(ctx context.Context, id int64) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deletedRules = append(s.deletedRules, id)
	return nil
}

func (s *ruleStore) GetMonitor(ctx context.Context, id int64) (*store.Monitor, error) {
	if s.getMonitorErr != nil {
		return nil, s.getMonitorErr
	}
	if id == 0 {
		return nil, store.ErrNotFound
	}
	return &store.Monitor{ID: id, Name: "test-monitor", ContractIDs: []string{"CABC"}}, nil
}

// CreateAuditEntry is a stub for the Audits interface.
func (s *ruleStore) CreateAuditEntry(ctx context.Context, e *store.AuditEntry) error {
	return nil
}

func testServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	reg := rules.NewRegistry()
	f := notify.DefaultFactory()
	srv := New(st, reg, f, &fakeRPC{}, discardLogger())
	return httptest.NewServer(srv.Routes())
}

func postJSONReq(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	res, err := http.Post(url, "application/json", bytes.NewReader(data))
	require.NoError(t, err)
	return res
}

func patchJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return res
}

func deleteReq(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return res
}

func getReq(t *testing.T, url string) *http.Response {
	t.Helper()
	res, err := http.Get(url)
	require.NoError(t, err)
	return res
}

func decodeBody(t *testing.T, res *http.Response, v any) {
	t.Helper()
	require.NoError(t, json.NewDecoder(res.Body).Decode(v))
	res.Body.Close()
}

func TestCreateRule_Success(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	res := postJSONReq(t, srv.URL+"/monitors/1/rules", map[string]any{
		"type":   "event_emitted",
		"params": map[string]any{"event_name": "transfer"},
		"enabled": true,
	})
	require.Equal(t, http.StatusCreated, res.StatusCode)

	var rule store.Rule
	decodeBody(t, res, &rule)
	require.Equal(t, "event_emitted", rule.Type)
	require.True(t, rule.Enabled)
	require.Equal(t, int64(1), rule.MonitorID)
	require.NotEmpty(t, rule.Params)
}

func TestCreateRule_MissingType(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	res := postJSONReq(t, srv.URL+"/monitors/1/rules", map[string]any{
		"params": map[string]any{},
	})
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Equal(t, "type is required", body["error"])
	details := body["details"].([]any)
	require.Len(t, details, 1)
	require.Equal(t, "type", details[0].(map[string]any)["field"])
}

func TestCreateRule_UnknownType(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	res := postJSONReq(t, srv.URL+"/monitors/1/rules", map[string]any{
		"type": "nonexistent_type",
		"params": map[string]any{},
	})
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Contains(t, body["error"], "unknown rule type")
}

func TestCreateRule_InvalidParams(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	// value_threshold requires comparison and threshold
	res := postJSONReq(t, srv.URL+"/monitors/1/rules", map[string]any{
		"type": "value_threshold",
		"params": map[string]any{
			"comparison": "invalid",
		},
	})
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Equal(t, "validation failed", body["error"])
	details := body["details"].([]any)
	require.Contains(t, details[0].(map[string]any)["field"], "params")
}

func TestCreateRule_InvalidSeverity(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	res := postJSONReq(t, srv.URL+"/monitors/1/rules", map[string]any{
		"type":     "event_emitted",
		"params":   map[string]any{"event_name": "transfer"},
		"severity": "invalid",
	})
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Contains(t, body["error"], "must be")
}

func TestCreateRule_MalformedJSON(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/monitors/1/rules", "application/json", bytes.NewReader([]byte("{invalid")))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	res.Body.Close()
}

func TestCreateRule_MonitorNotFound(t *testing.T) {
	st := &ruleStore{}
	st.getMonitorErr = store.ErrNotFound
	srv := testServer(t, st)
	defer srv.Close()

	res := postJSONReq(t, srv.URL+"/monitors/999/rules", map[string]any{
		"type":   "event_emitted",
		"params": map[string]any{"event_name": "transfer"},
	})
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestListRules_Success(t *testing.T) {
	st := &ruleStore{
		listRules: []store.Rule{
			{ID: 1, MonitorID: 1, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true},
			{ID: 2, MonitorID: 1, Type: "value_threshold", Params: json.RawMessage(`{"comparison":"gt","threshold":100}`), Enabled: true},
		},
	}
	srv := testServer(t, st)
	defer srv.Close()

	res := getReq(t, srv.URL+"/monitors/1/rules")
	require.Equal(t, http.StatusOK, res.StatusCode)

	var rules []store.Rule
	decodeBody(t, res, &rules)
	require.Len(t, rules, 2)
}

func TestListRules_Empty(t *testing.T) {
	st := &ruleStore{listRules: nil}
	srv := testServer(t, st)
	defer srv.Close()

	res := getReq(t, srv.URL+"/monitors/1/rules")
	require.Equal(t, http.StatusOK, res.StatusCode)

	var rules []store.Rule
	decodeBody(t, res, &rules)
	require.NotNil(t, rules)
	require.Len(t, rules, 0)
}

func TestUpdateRule_Success(t *testing.T) {
	existing := &store.Rule{ID: 1, MonitorID: 1, Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true}
	st := &ruleStore{getRule: existing}
	srv := testServer(t, st)
	defer srv.Close()

	res := patchJSON(t, srv.URL+"/monitors/1/rules/1", map[string]any{
		"enabled": false,
	})
	require.Equal(t, http.StatusOK, res.StatusCode)

	var rule store.Rule
	decodeBody(t, res, &rule)
	require.False(t, rule.Enabled)
	require.Len(t, st.updatedRules, 1)
	require.False(t, st.updatedRules[0].Enabled)
}

func TestUpdateRule_ParamsUpdate(t *testing.T) {
	existing := &store.Rule{ID: 1, MonitorID: 1, Type: "value_threshold", Params: json.RawMessage(`{"comparison":"gt","threshold":100}`), Enabled: true}
	st := &ruleStore{getRule: existing}
	srv := testServer(t, st)
	defer srv.Close()

	res := patchJSON(t, srv.URL+"/monitors/1/rules/1", map[string]any{
		"params": map[string]any{"comparison": "gte", "threshold": 200},
	})
	require.Equal(t, http.StatusOK, res.StatusCode)

	var rule store.Rule
	decodeBody(t, res, &rule)
	var params map[string]any
	json.Unmarshal(rule.Params, &params)
	require.Equal(t, "gte", params["comparison"])
	require.Equal(t, float64(200), params["threshold"])
}

func TestUpdateRule_InvalidParams(t *testing.T) {
	existing := &store.Rule{ID: 1, MonitorID: 1, Type: "value_threshold", Params: json.RawMessage(`{"comparison":"gt","threshold":100}`), Enabled: true}
	st := &ruleStore{getRule: existing}
	srv := testServer(t, st)
	defer srv.Close()

	res := patchJSON(t, srv.URL+"/monitors/1/rules/1", map[string]any{
		"params": map[string]any{"comparison": "invalid"},
	})
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Equal(t, "validation failed", body["error"])
}

func TestUpdateRule_UnknownType(t *testing.T) {
	existing := &store.Rule{ID: 1, MonitorID: 1, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	st := &ruleStore{getRule: existing}
	srv := testServer(t, st)
	defer srv.Close()

	res := patchJSON(t, srv.URL+"/monitors/1/rules/1", map[string]any{
		"type": "unknown_type",
	})
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Contains(t, body["error"], "unknown rule type")
}

func TestUpdateRule_NotFound(t *testing.T) {
	st := &ruleStore{getRuleErr: store.ErrNotFound}
	srv := testServer(t, st)
	defer srv.Close()

	res := patchJSON(t, srv.URL+"/monitors/1/rules/999", map[string]any{
		"enabled": false,
	})
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestUpdateRule_WrongMonitor(t *testing.T) {
	existing := &store.Rule{ID: 1, MonitorID: 2, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	st := &ruleStore{getRule: existing}
	srv := testServer(t, st)
	defer srv.Close()

	res := patchJSON(t, srv.URL+"/monitors/1/rules/1", map[string]any{
		"enabled": false,
	})
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestDeleteRule_Success(t *testing.T) {
	existing := &store.Rule{ID: 1, MonitorID: 1, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	st := &ruleStore{getRule: existing}
	srv := testServer(t, st)
	defer srv.Close()

	res := deleteReq(t, srv.URL+"/monitors/1/rules/1")
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	res.Body.Close()

	require.Len(t, st.deletedRules, 1)
	require.Equal(t, int64(1), st.deletedRules[0])
}

func TestDeleteRule_NotFound(t *testing.T) {
	st := &ruleStore{getRuleErr: store.ErrNotFound}
	srv := testServer(t, st)
	defer srv.Close()

	res := deleteReq(t, srv.URL+"/monitors/1/rules/999")
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestDeleteRule_WrongMonitor(t *testing.T) {
	existing := &store.Rule{ID: 1, MonitorID: 2, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	st := &ruleStore{getRule: existing}
	srv := testServer(t, st)
	defer srv.Close()

	res := deleteReq(t, srv.URL+"/monitors/1/rules/1")
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestCreateRulesBulk_Success_CreatesMultiple(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	res := postJSONReq(t, srv.URL+"/monitors/1/rules/bulk", []map[string]any{
		{"type": "event_emitted", "params": map[string]any{"event_name": "transfer"}},
		{"type": "value_threshold", "params": map[string]any{"comparison": "gt", "threshold": 100}},
	})
	require.Equal(t, http.StatusCreated, res.StatusCode)

	var rules []store.Rule
	decodeBody(t, res, &rules)
	require.Len(t, rules, 2)
	require.Equal(t, "event_emitted", rules[0].Type)
	require.Equal(t, "value_threshold", rules[1].Type)
}

func TestCreateRulesBulk_Empty(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	res := postJSONReq(t, srv.URL+"/monitors/1/rules/bulk", []map[string]any{})
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Contains(t, body["error"], "must not be empty")
}

func TestCreateRulesBulk_TooMany(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	items := make([]map[string]any, 51)
	for i := range items {
		items[i] = map[string]any{"type": "event_emitted", "params": map[string]any{"event_name": "e"}}
	}
	res := postJSONReq(t, srv.URL+"/monitors/1/rules/bulk", items)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Contains(t, body["error"], "at most 50")
}

func TestCreateRulesBulk_PartialFailure(t *testing.T) {
	st := &ruleStore{}
	srv := testServer(t, st)
	defer srv.Close()

	// First valid, second invalid (missing threshold)
	res := postJSONReq(t, srv.URL+"/monitors/1/rules/bulk", []map[string]any{
		{"type": "event_emitted", "params": map[string]any{"event_name": "transfer"}},
		{"type": "value_threshold", "params": map[string]any{"comparison": "invalid"}},
	})
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	var body map[string]any
	decodeBody(t, res, &body)
	require.Equal(t, "validation failed", body["error"])
	details := body["details"].([]any)
	// Both comparison and threshold fail validation
	require.Len(t, details, 2)
	require.Contains(t, details[0].(map[string]any)["field"], "rules[1]")
}

func TestCreateRulesBulk_MonitorNotFound(t *testing.T) {
	st := &ruleStore{}
	st.getMonitorErr = store.ErrNotFound
	srv := testServer(t, st)
	defer srv.Close()

	res := postJSONReq(t, srv.URL+"/monitors/999/rules/bulk", []map[string]any{
		{"type": "event_emitted", "params": map[string]any{"event_name": "transfer"}},
	})
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

// TestCreateRule_AllRegisteredTypes tests that all registered rule types can be created
// with valid params. Some types (contract_allowlist, address_watchlist) require
// valid strkeys and are tested separately.
func TestCreateRule_AllRegisteredTypes(t *testing.T) {
	// Types that can be tested with simple params
	simpleTypes := []string{
		rules.TypeEventEmitted,
		rules.TypeValueThreshold,
		rules.TypeEventNameGlob,
		rules.TypeNumericRange,
		rules.TypeTokenSupplyChange,
		rules.TypeFrequencyThreshold,
		rules.TypeTopicRegex,
		rules.TypeTopicPosition,
		rules.TypeComposite,
		rules.TypeAbsenceOfEvent,
	}

	for _, ruleType := range simpleTypes {
		t.Run(ruleType, func(t *testing.T) {
			st := &ruleStore{}
			srv := testServer(t, st)
			defer srv.Close()

			params := validParamsForType(t, ruleType)
			res := postJSONReq(t, srv.URL+"/monitors/1/rules", map[string]any{
				"type":   ruleType,
				"params": params,
			})
			require.Equal(t, http.StatusCreated, res.StatusCode, "type=%s", ruleType)

			var rule store.Rule
			decodeBody(t, res, &rule)
			require.Equal(t, ruleType, rule.Type)
		})
	}
}

// TestCreateRule_ComplexTypes tests rule types that require valid strkeys.
func TestCreateRule_ComplexTypes(t *testing.T) {
	// contract_allowlist requires valid contract IDs (strkey starting with C)
	// address_watchlist requires valid account addresses (strkey starting with G)
	// These are tested in their respective package tests (contract_allowlist_test.go,
	// address_watchlist_test.go) which generate valid strkeys.
}

func validParamsForType(t *testing.T, ruleType string) map[string]any {
	t.Helper()
	// Use valid contract IDs from contract_allowlist_test.go
	const (
		allowTreasury = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
		allowOther    = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	)
	switch ruleType {
	case rules.TypeEventEmitted:
		return map[string]any{"event_name": "transfer"}
	case rules.TypeValueThreshold:
		return map[string]any{"comparison": "gt", "threshold": "1000000"}
	case rules.TypeContractAllowlist:
		return map[string]any{"contract_ids": []string{allowTreasury, allowOther}}
	case rules.TypeEventNameGlob:
		return map[string]any{"patterns": []string{"transfer*"}}
	case rules.TypeNumericRange:
		return map[string]any{"field": "amount", "min": "0", "max": "1000"}
	case rules.TypeTokenSupplyChange:
		return map[string]any{"asset": "native"}
	case rules.TypeFrequencyThreshold:
		return map[string]any{"count": 5, "window": "5m"}
	case rules.TypeTopicRegex:
		return map[string]any{"topic_index": 1, "pattern": "^[A-Z]+$"}
	case rules.TypeAddressWatchlist:
		// AddressWatchlist requires valid Stellar addresses (strkey starting with G)
		return map[string]any{"addresses": []string{"GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"}}
	case rules.TypeTopicPosition:
		return map[string]any{"position": 1, "equals": "POOL_USDC_XLM"}
	case rules.TypeComposite:
		return map[string]any{"op": "and", "rules": []map[string]any{
			{"type": "event_emitted", "params": map[string]any{"event_name": "transfer"}},
		}}
	case rules.TypeAbsenceOfEvent:
		return map[string]any{"event_name": "heartbeat", "window": "30m"}
	default:
		t.Fatalf("unknown rule type: %s", ruleType)
		return nil
	}
}