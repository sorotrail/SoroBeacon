package api

import (
	"net/http"
	"sort"

	"github.com/sorotrail/sorobeacon/internal/rules"
)

type ruleTypeResponse struct {
	Type       string              `json:"type"`
	Parameters []rules.FieldSchema `json:"parameters"`
}

func (s *Server) listRuleTypes(w http.ResponseWriter, r *http.Request) {
	schemas := s.registry.AllSchemas()
	types := s.registry.Types()
	sort.Strings(types)

	items := make([]ruleTypeResponse, 0, len(types))
	for _, name := range types {
		items = append(items, ruleTypeResponse{
			Type:       name,
			Parameters: schemas[name],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rule_types": items})
}
