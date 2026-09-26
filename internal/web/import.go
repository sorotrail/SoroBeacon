package web

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

const maxWebImportRows = 500

// importContractsWeb handles the dashboard file upload for bulk contract import.
func (s *Server) importContractsWeb(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "invalid upload", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file is required", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		http.Error(w, "failed to read file", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	if name == "" {
		name = "imported"
	}

	var ids []string
	var jsonInput struct {
		ContractIDs []string `json:"contract_ids"`
	}
	if err := json.Unmarshal(data, &jsonInput); err == nil && len(jsonInput.ContractIDs) > 0 {
		ids = jsonInput.ContractIDs
	} else {
		parsed, err := parseWebCSV(strings.NewReader(string(data)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ids = parsed
	}

	if len(ids) == 0 {
		http.Error(w, "no contract IDs found in file", http.StatusBadRequest)
		return
	}
	if len(ids) > maxWebImportRows {
		http.Error(w, fmt.Sprintf("at most %d contracts per import", maxWebImportRows), http.StatusBadRequest)
		return
	}

	for i, cid := range ids {
		ids[i] = strings.TrimSpace(cid)
		if !stellar.IsValidContractID(ids[i]) {
			http.Error(w, fmt.Sprintf("invalid contract ID on row %d: %s", i+1, ids[i]), http.StatusBadRequest)
			return
		}
	}

	existing, _ := s.store.ListMonitors(r.Context(), false)
	existingContracts := make(map[string]bool)
	for _, m := range existing {
		for _, cid := range m.ContractIDs {
			existingContracts[cid] = true
		}
	}

	created := 0
	for _, cid := range ids {
		if existingContracts[cid] {
			continue
		}
		suffix := cid
		if len(cid) > 12 {
			suffix = cid[:6] + cid[len(cid)-6:]
		}
		m := store.Monitor{
			Name:        fmt.Sprintf("%s-%s", name, suffix),
			ContractIDs: []string{cid},
			Enabled:     true,
		}
		if err := s.store.CreateMonitor(r.Context(), &m); err != nil {
			s.log.Error("web_import", "contract_id", cid, "err", err)
			continue
		}
		created++
	}

	s.log.Info("web_import", "total", len(ids), "created", created)
	http.Redirect(w, r, "/monitors", http.StatusSeeOther)
}

func parseWebCSV(r io.Reader) ([]string, error) {
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
		if len(ids) > maxWebImportRows {
			return nil, fmt.Errorf("at most %d contracts per import", maxWebImportRows)
		}
	}
	return ids, nil
}
