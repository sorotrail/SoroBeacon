package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

const dupSecretURL = "https://hooks.example.com/services/SECRET-TOKEN-123"

// sqliteAPI serves the real router over a real SQLite store, so ETags and the
// bulk path are exercised end to end rather than against a fake.
func sqliteAPI(t *testing.T) (*httptest.Server, store.Store, *bytes.Buffer) {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	if err := store.Migrate(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.NewSQLite(context.Background(), url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	buf, log := newLogBuf()
	srv := httptest.NewServer(New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, log).Routes())
	t.Cleanup(srv.Close)
	return srv, st, buf
}

func mkChannel(t *testing.T, st store.Store, name string, enabled bool) *store.Channel {
	t.Helper()
	ch := &store.Channel{
		Name: name, Type: "webhook", Enabled: enabled,
		Config: json.RawMessage(`{"url":"` + dupSecretURL + `"}`),
	}
	if err := st.CreateChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

func do(t *testing.T, method, url, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res, raw
}

func TestListChannels_ETagNotModifiedAndBulkChangesIt(t *testing.T) {
	srv, st, _ := sqliteAPI(t)
	a := mkChannel(t, st, "a", true)
	mkChannel(t, st, "b", true)

	res, _ := do(t, "GET", srv.URL+"/channels", "", nil)
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on list response")
	}
	if cc := res.Header.Get("Cache-Control"); cc == "no-store" {
		t.Fatalf("Cache-Control = %q: no-store makes the ETag useless", cc)
	}

	res, body := do(t, "GET", srv.URL+"/channels", "", map[string]string{"If-None-Match": etag})
	if res.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("matching If-None-Match = %d with %d body bytes, want 304 and none", res.StatusCode, len(body))
	}

	res, _ = do(t, "POST", srv.URL+"/channels/bulk", `{"ids":[`+itoa(a.ID)+`],"enabled":false}`, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bulk = %d", res.StatusCode)
	}
	res, _ = do(t, "GET", srv.URL+"/channels", "", map[string]string{"If-None-Match": etag})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("after bulk update = %d, want 200 (ETag must change)", res.StatusCode)
	}
	if res.Header.Get("ETag") == etag {
		t.Fatal("ETag unchanged after bulk update")
	}
}

func TestListMonitors_ETagNotModified(t *testing.T) {
	srv, _, _ := sqliteAPI(t)
	res, _ := do(t, "GET", srv.URL+"/monitors", "", nil)
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on monitors list")
	}
	res, body := do(t, "GET", srv.URL+"/monitors", "", map[string]string{"If-None-Match": etag})
	if res.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("= %d with %d bytes, want 304 and none", res.StatusCode, len(body))
	}
}

func TestBulkChannels_UnknownIDsReportedKnownApplied(t *testing.T) {
	srv, st, _ := sqliteAPI(t)
	a := mkChannel(t, st, "a", true)

	res, raw := do(t, "POST", srv.URL+"/channels/bulk", `{"ids":[`+itoa(a.ID)+`,9999],"enabled":false}`, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Updated    int     `json:"updated"`
		UnknownIDs []int64 `json:"unknown_ids"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Updated != 1 || len(out.UnknownIDs) != 1 || out.UnknownIDs[0] != 9999 {
		t.Fatalf("got %+v", out)
	}
	got, _ := st.GetChannel(context.Background(), a.ID)
	if got.Enabled {
		t.Fatal("known channel not disabled")
	}

	res, _ = do(t, "POST", srv.URL+"/channels/bulk", `{"ids":[],"enabled":true}`, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty ids = %d, want 400", res.StatusCode)
	}
}

func TestDuplicateChannel_DisabledNoAttachmentsNoSecretLogged(t *testing.T) {
	srv, st, logs := sqliteAPI(t)
	src := mkChannel(t, st, "ops", true)
	m := &store.Monitor{Name: "m", ContractIDs: []string{validContract}, Enabled: true, Priority: store.PriorityNormal}
	if err := st.CreateMonitor(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMonitorChannels(context.Background(), m.ID, []int64{src.ID}); err != nil {
		t.Fatal(err)
	}

	res, raw := do(t, "POST", srv.URL+"/channels/"+itoa(src.ID)+"/duplicate", "", nil)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", res.StatusCode, raw)
	}
	var dup store.Channel
	if err := json.Unmarshal(raw, &dup); err != nil {
		t.Fatal(err)
	}
	if dup.Enabled || dup.Name != "ops (copy)" || dup.ID == src.ID {
		t.Fatalf("dup = %+v", dup)
	}
	attached, err := st.ListMonitorsForChannel(context.Background(), dup.ID)
	if err != nil || len(attached) != 0 {
		t.Fatalf("copy attachments = %v, %v; want none", attached, err)
	}
	stored, _ := st.GetChannel(context.Background(), dup.ID)
	if !strings.Contains(string(stored.Config), dupSecretURL) {
		t.Fatal("config was not carried to the copy")
	}
	if strings.Contains(string(raw), "SECRET-TOKEN") || strings.Contains(logs.String(), "SECRET-TOKEN") {
		t.Fatal("credential leaked into the response or the log")
	}

	res, _ = do(t, "POST", srv.URL+"/channels/"+itoa(src.ID)+"/duplicate", "", nil)
	var second store.Channel
	_ = json.NewDecoder(res.Body).Decode(&second)
	res, raw = do(t, "POST", srv.URL+"/channels/9999/duplicate", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id = %d: %s", res.StatusCode, raw)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
