package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type importStore struct {
	fakeStore
	monitors []store.Monitor
	nextID   int64
	channels map[int64][]int64
}

func (s *importStore) CreateMonitor(_ context.Context, m *store.Monitor) error {
	s.nextID++
	m.ID = s.nextID
	s.monitors = append(s.monitors, *m)
	return nil
}

func (s *importStore) ListMonitors(_ context.Context, _ bool) ([]store.Monitor, error) {
	return s.monitors, nil
}

func (s *importStore) SetMonitorChannels(_ context.Context, monitorID int64, channelIDs []int64) error {
	if s.channels == nil {
		s.channels = make(map[int64][]int64)
	}
	s.channels[monitorID] = channelIDs
	return nil
}

func TestImportContractsJSON(t *testing.T) {
	st := &importStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"batch","contract_ids":["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"]}`
	res, err := http.Post(srv.URL+"/monitors/import", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var result struct {
		Total   int `json:"total"`
		Results []struct {
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Errorf("total = %d, want 1", result.Total)
	}
	if len(result.Results) != 1 || result.Results[0].Status != "created" {
		t.Errorf("unexpected results: %+v", result.Results)
	}
}

func TestImportContractsCSV(t *testing.T) {
	st := &importStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	csv := "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC\n"
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/monitors/import?name=csv-batch", bytes.NewBufferString(csv))
	req.Header.Set("Content-Type", "text/csv")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

func TestImportContractsInvalidID(t *testing.T) {
	st := &importStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"bad","contract_ids":["not-a-contract"]}`
	res, err := http.Post(srv.URL+"/monitors/import", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestImportContractsDuplicateSkip(t *testing.T) {
	st := &importStore{
		monitors: []store.Monitor{
			{ID: 1, Name: "existing", ContractIDs: []string{"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"}},
		},
		nextID: 1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"dup","contract_ids":["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"]}`
	res, err := http.Post(srv.URL+"/monitors/import", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var result struct {
		Results []struct {
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != "skipped" {
		t.Errorf("expected skipped, got %+v", result.Results)
	}
}

func TestImportEmptyBody(t *testing.T) {
	st := &importStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"empty","contract_ids":[]}`
	res, err := http.Post(srv.URL+"/monitors/import", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}
