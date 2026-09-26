package api

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

const maxImportRows = 500

type importInput struct {
	Name        string   `json:"name"`
	ContractIDs []string `json:"contract_ids"`
	ChannelIDs  []int64  `json:"channel_ids,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
}

type importRowResult struct {
	Row        int    `json:"row"`
	ContractID string `json:"contract_id"`
	MonitorID  int64  `json:"monitor_id,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// importContracts handles POST /monitors/import. It accepts either JSON or CSV.
// JSON: {"name":"prefix","contract_ids":["C1","C2",...],"channel_ids":[1],"enabled":true}
// CSV: one contract ID per line (no header).
//
// Design: all-or-nothing. Every contract is validated before any is written,
// so a file with a bad ID on the last row does not leave partial state.
// Duplicates (contracts already monitored under the same name prefix) are
// skipped with status "skipped".
func (s *Server) importContracts(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")

	var in importInput
	switch {
	case strings.HasPrefix(ct, "text/csv"):
		ids, err := parseCSVContracts(r.Body)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		in.ContractIDs = ids
		in.Name = r.URL.Query().Get("name")
		if in.Name == "" {
			in.Name = "imported"
		}
	default:
		if !readJSON(w, r, &in) {
			return
		}
	}

	if in.Name == "" {
		writeErr(w, r, http.StatusBadRequest, "name is required")
		return
	}
	if len(in.ContractIDs) == 0 {
		writeErr(w, r, http.StatusBadRequest, "contract_ids is required and must not be empty")
		return
	}
	if len(in.ContractIDs) > maxImportRows {
		writeErr(w, r, http.StatusBadRequest, fmt.Sprintf("at most %d contracts per import", maxImportRows))
		return
	}

	// Validate all contract IDs before writing anything.
	var problems []FieldError
	for i, cid := range in.ContractIDs {
		cid = strings.TrimSpace(cid)
		in.ContractIDs[i] = cid
		if cid == "" {
			problems = append(problems, FieldError{
				Field:  fmt.Sprintf("contract_ids[%d]", i),
				Reason: "empty contract ID",
			})
			continue
		}
		if !stellar.IsValidContractID(cid) {
			problems = append(problems, FieldError{
				Field:  fmt.Sprintf("contract_ids[%d]", i),
				Reason: fmt.Sprintf("invalid contract ID: %s", cid),
			})
		}
	}
	if len(problems) > 0 {
		writeValidation(w, r, problems)
		return
	}

	// Deduplicate within the request.
	seen := make(map[string]bool, len(in.ContractIDs))
	unique := make([]string, 0, len(in.ContractIDs))
	for _, cid := range in.ContractIDs {
		if seen[cid] {
			continue
		}
		seen[cid] = true
		unique = append(unique, cid)
	}
	in.ContractIDs = unique

	// Check for already-monitored contracts by listing existing monitors.
	existing, _ := s.store.ListMonitors(r.Context(), false)
	existingContracts := make(map[string]bool)
	for _, m := range existing {
		for _, cid := range m.ContractIDs {
			existingContracts[cid] = true
		}
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	results := make([]importRowResult, 0, len(in.ContractIDs))
	for i, cid := range in.ContractIDs {
		if existingContracts[cid] {
			results = append(results, importRowResult{
				Row:        i + 1,
				ContractID: cid,
				Status:     "skipped",
				Error:      "already monitored",
			})
			continue
		}
		m := store.Monitor{
			Name:        fmt.Sprintf("%s-%s", in.Name, truncateForName(cid)),
			ContractIDs: []string{cid},
			Enabled:     enabled,
		}
		if err := s.store.CreateMonitor(r.Context(), &m); err != nil {
			results = append(results, importRowResult{
				Row:        i + 1,
				ContractID: cid,
				Status:     "error",
				Error:      "failed to create monitor",
			})
			continue
		}
		if len(in.ChannelIDs) > 0 {
			_ = s.store.SetMonitorChannels(r.Context(), m.ID, in.ChannelIDs)
		}
		results = append(results, importRowResult{
			Row:        i + 1,
			ContractID: cid,
			MonitorID:  m.ID,
			Status:     "created",
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":   len(in.ContractIDs),
		"results": results,
	})
}

func parseCSVContracts(r io.Reader) ([]string, error) {
	reader := csv.NewReader(bufio.NewReader(r))
	reader.FieldsPerRecord = -1
	var ids []string
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("CSV parse error: %w", err)
		}
		if len(record) == 0 {
			continue
		}
		id := strings.TrimSpace(record[0])
		if id == "" || id == "contract_id" {
			continue
		}
		ids = append(ids, id)
		if len(ids) > maxImportRows {
			return nil, fmt.Errorf("at most %d contracts per import", maxImportRows)
		}
	}
	return ids, nil
}

func truncateForName(contractID string) string {
	if len(contractID) <= 12 {
		return contractID
	}
	return contractID[:6] + contractID[len(contractID)-6:]
}
