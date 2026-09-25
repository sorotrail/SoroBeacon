package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
)

func TestListRuleTypesIncludesEveryRegisteredType(t *testing.T) {
	registry := rules.NewRegistry()
	s := New(&fakeStore{}, registry, notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/rule-types", nil)
	res := httptest.NewRecorder()
	s.Routes().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("GET /rule-types = %d, want 200", res.Code)
	}
	var body struct {
		RuleTypes []ruleTypeResponse `json:"rule_types"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.RuleTypes) != len(registry.Types()) {
		t.Fatalf("got %d rule types, want %d", len(body.RuleTypes), len(registry.Types()))
	}

	got := make(map[string]ruleTypeResponse, len(body.RuleTypes))
	for _, item := range body.RuleTypes {
		got[item.Type] = item
		if len(item.Parameters) == 0 {
			t.Errorf("rule type %q has no parameter schema", item.Type)
		}
		for _, field := range item.Parameters {
			if field.Name == "" || field.Type == "" || field.Description == "" {
				t.Errorf("rule type %q has incomplete field schema: %+v", item.Type, field)
			}
		}
	}
	for _, name := range registry.Types() {
		if _, ok := got[name]; !ok {
			t.Errorf("registered rule type %q missing from response", name)
		}
	}
}
