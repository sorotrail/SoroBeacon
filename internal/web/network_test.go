package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// networkPagingStore records the filters listings saw and the monitor the
// create form would write, so a test can tell "the form value reached the
// store" from "the template rendered something that looked right".
type networkPagingStore struct {
	emptyStore
	gotFilter store.ListFilter
	created   store.Monitor
	statuses  []poller.NetworkStatus
}

func (n *networkPagingStore) ListMonitorsPage(_ context.Context, f store.ListFilter) ([]store.Monitor, error) {
	n.gotFilter = f
	all := []store.Monitor{
		{ID: 1, Name: "test watch", Network: "testnet", ContractIDs: []string{"C"}, Enabled: true},
		{ID: 2, Name: "main watch", Network: "mainnet", ContractIDs: []string{"C"}, Enabled: true},
	}
	if f.Network == "" {
		return all, nil
	}
	var out []store.Monitor
	for _, m := range all {
		if m.Network == f.Network {
			out = append(out, m)
		}
	}
	return out, nil
}

func (n *networkPagingStore) ListAlerts(_ context.Context, f store.AlertFilter) ([]store.Alert, error) {
	return []store.Alert{{ID: 1, MonitorID: 1, Network: "testnet"}}, nil
}

func (n *networkPagingStore) CreateMonitor(_ context.Context, m *store.Monitor) error {
	m.ID = 1
	n.created = *m
	return nil
}

func (n *networkPagingStore) Statuses(context.Context) []poller.NetworkStatus { return n.statuses }

// Position keeps the fake a stand-in for the supervisor: WithPoller takes the
// one object and type-asserts the per-network view out of it.
func (n *networkPagingStore) Position() poller.Position { return poller.Position{} }

func newNetworkServer(t *testing.T, st store.Store, networks []string, p PositionReader) *Server {
	t.Helper()
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.WithNetworks(networks)
	if p != nil {
		s.WithPoller(p)
	}
	return s
}

func get(t *testing.T, s *Server, path string) string {
	t.Helper()
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", path, res.StatusCode, body)
	}
	return string(body)
}

// TestNetworkChoice: the form offers configured names, but a hand-built POST
// must not create a monitor on a chain nothing polls. Falling back to the
// primary keeps a dashboard submission working, and is the same default an
// omitted field gets.
func TestNetworkChoice(t *testing.T) {
	multi := newNetworkServer(t, &networkPagingStore{}, []string{"testnet", "mainnet"}, nil)
	if got := multi.networkChoice(""); got != "testnet" {
		t.Fatalf("blank = %q, want the primary", got)
	}
	if got := multi.networkChoice(" MAINNET "); got != "mainnet" {
		t.Fatalf("spaced/upper = %q, want the trimmed lower name", got)
	}
	if got := multi.networkChoice("futurenet"); got != "testnet" {
		t.Fatalf("unpolled chain = %q, want the primary", got)
	}
	// No list configured is the single-network instance: monitors stay
	// unlabelled exactly as they were before networks existed.
	single := newNetworkServer(t, &networkPagingStore{}, nil, nil)
	if got := single.networkChoice("testnet"); got != "" {
		t.Fatalf("single-network choice = %q, want unlabelled", got)
	}
}

// TestMonitorPageHidesNetworkPickerForSingleInstance and its counterpart pin
// the two shapes of the create form: an operator polling one chain should not
// be asked to name it, and one polling several must say which.
func TestMonitorCreateFormNetworkPicker(t *testing.T) {
	single := get(t, newNetworkServer(t, &networkPagingStore{}, nil, nil), "/monitors")
	if strings.Contains(single, `name="network"`) {
		t.Fatalf("single-network form shows a network picker:\n%s", single)
	}

	multi := get(t, newNetworkServer(t, &networkPagingStore{}, []string{"testnet", "mainnet"}, nil), "/monitors")
	if !strings.Contains(multi, `name="network"`) {
		t.Fatalf("multi-network form has no network picker:\n%s", multi)
	}
	for _, want := range []string{`<option value="testnet" selected>`, `<option value="mainnet" >`} {
		if !strings.Contains(multi, want) {
			t.Fatalf("multi-network form missing %q:\n%s", want, multi)
		}
	}
}

// TestMonitorFilterSelectAndPaging: a filtered listing whose pagination link
// drops the filter silently changes the question mid-page — the operator
// starts seeing other chains' monitors on page two.
func TestMonitorFilterSelectAndPaging(t *testing.T) {
	st := &networkPagingStore{}
	html := get(t, newNetworkServer(t, st, []string{"testnet", "mainnet"}, nil), "/monitors?network=mainnet")
	if st.gotFilter.Network != "mainnet" {
		t.Fatalf("store filter network = %q, want mainnet", st.gotFilter.Network)
	}
	if !strings.Contains(html, `<option value="mainnet" selected>`) {
		t.Fatalf("filter select did not keep the chosen chain:\n%s", html)
	}
	if !strings.Contains(html, "main watch") || strings.Contains(html, "test watch") {
		t.Fatalf("the Network column should show only the filtered chain's own value:\n%s", html)
	}
}

// TestMonitorPagingKeepsNetwork: the Older link is built from the active
// filters, and one that drops `network` silently changes the question on page
// two — the operator starts seeing other chains' monitors mid-listing.
func TestMonitorPagingKeepsNetwork(t *testing.T) {
	html := get(t, newNetworkServer(t, &fullPageNetworkStore{}, []string{"testnet", "mainnet"}, nil),
		"/monitors?network=mainnet&q=watch")
	start := strings.Index(html, `<a href="/monitors?`)
	if start < 0 {
		t.Fatalf("no Older link rendered at all:\n%s", html)
	}
	links := html[start:]
	if !strings.Contains(links, "cursor=50") {
		t.Fatalf("expected an Older link with a cursor, got:\n%s", links)
	}
	for _, want := range []string{"network=mainnet", "q=watch"} {
		if !strings.Contains(links, want) {
			t.Fatalf("Older link dropped %s:\n%s", want, links)
		}
	}
}

// fullPageNetworkStore fills a page so the listing always emits a cursor.
type fullPageNetworkStore struct {
	networkPagingStore
}

func (fullPageNetworkStore) ListMonitorsPage(context.Context, store.ListFilter) ([]store.Monitor, error) {
	out := make([]store.Monitor, 50)
	for i := range out {
		out[i] = store.Monitor{ID: int64(i + 1), Name: "watch", Network: "mainnet", Enabled: true}
	}
	return out, nil
}

// TestMonitorTableHasNoNetworkColumnForSingleInstance: one chain per instance
// makes the column a constant, and a constant is noise.
func TestMonitorTableHasNoNetworkColumnForSingleInstance(t *testing.T) {
	html := get(t, newNetworkServer(t, &networkPagingStore{}, nil, nil), "/monitors")
	if strings.Contains(html, ">Network<") {
		t.Fatalf("single-network listing renders a Network column:\n%s", html)
	}
}

// TestMonitorCreateWritesNetwork is the dashboard half of "a monitor is born on
// a chain": the select's value must reach the row, not be dropped on the floor.
func TestMonitorCreateWritesNetwork(t *testing.T) {
	st := &networkPagingStore{}
	s := newNetworkServer(t, st, []string{"testnet", "mainnet"}, nil)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	form := url.Values{
		"name":         {"main watch"},
		"contract_ids": {"CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"},
		"network":      {"mainnet"},
	}
	res, err := http.PostForm(srv.URL+"/monitors", form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if st.created.Network != "mainnet" {
		t.Fatalf("created monitor network = %q, want mainnet", st.created.Network)
	}
}

// TestIndexShowsPerNetworkIngestTable: with several chains, the overview's
// single lag number is the worst chain's. Without the table an operator cannot
// tell which one is behind, which is the whole reason to run one process.
func TestIndexShowsPerNetworkIngestTable(t *testing.T) {
	st := &networkPagingStore{statuses: []poller.NetworkStatus{
		{Network: "testnet", Source: "ok", LastProcessedLedger: 100, LatestChainLedger: 101, LedgerLag: 1},
		{Network: "mainnet", Source: "connection refused", LastProcessedLedger: 500, LatestChainLedger: 900, LedgerLag: 400},
	}}
	s := newNetworkServer(t, st, []string{"testnet", "mainnet"}, st)
	html := get(t, s, "/")
	for _, want := range []string{"<h2>Networks</h2>", "testnet", "mainnet", `title="connection refused"`, ">400<"} {
		if !strings.Contains(html, want) {
			t.Fatalf("overview missing %q for the per-network table:\n%s", want, html)
		}
	}
}

// TestIndexOmitsNetworkTableForSingleInstance keeps the pre-multi-network
// overview unchanged: nothing to compare, so no table.
func TestIndexOmitsNetworkTableForSingleInstance(t *testing.T) {
	st := &networkPagingStore{statuses: []poller.NetworkStatus{{Network: "testnet", Source: "ok"}}}
	html := get(t, newNetworkServer(t, st, []string{"testnet"}, st), "/")
	if strings.Contains(html, "<h2>Networks</h2>") {
		t.Fatalf("single-chain overview renders a networks table:\n%s", html)
	}
}

// TestMonitorPageExplainsNetwork: an operator who expects to move a monitor to
// another chain needs the edit page to say it cannot be done, and what to do
// instead, before they hunt for the control.
func TestMonitorPageExplainsNetwork(t *testing.T) {
	html := get(t, newNetworkServer(t, &networkDetailStore{}, []string{"testnet", "mainnet"}, nil), "/monitors/1")
	if !strings.Contains(html, "testnet") || !strings.Contains(html, "delete and recreate") {
		t.Fatalf("monitor page does not state the network and that it is fixed:\n%s", html)
	}
}

// networkDetailStore serves one monitor to the detail page.
type networkDetailStore struct {
	networkPagingStore
}

func (n networkDetailStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	return &store.Monitor{ID: 1, Name: "test watch", Network: "testnet", ContractIDs: []string{"C"}}, nil
}

func (n networkDetailStore) ListRules(context.Context, int64, bool) ([]store.Rule, error) {
	return nil, nil
}

func (n networkDetailStore) ListMonitorChannels(context.Context, int64) ([]int64, error) {
	return nil, nil
}
