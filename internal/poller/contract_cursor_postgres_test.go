package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type postgresFailingAlertStore struct {
	Store
	failEvent string
}

func (s postgresFailingAlertStore) CreateAlert(ctx context.Context, alert *store.Alert) (store.AlertOutcome, error) {
	if alert.EventID == s.failEvent {
		return "", errors.New("injected alert persistence failure")
	}
	return s.Store.CreateAlert(ctx, alert)
}

func TestPostgresFailedEventDoesNotAdvanceContractCursor(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres poller integration test")
	}
	ctx := context.Background()
	require.NoError(t, store.Migrate(url))
	st, err := store.NewPostgres(ctx, url, store.PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)

	network := fmt.Sprintf("cursor-test-%d", time.Now().UnixNano())
	contractA := contractID(byte(time.Now().UnixNano()))
	contractB := contractID(byte(time.Now().UnixNano() >> 8))
	monitor := &store.Monitor{
		Name: "cursor-failure-test", ContractIDs: []string{contractA, contractB}, Enabled: true, Network: network,
	}
	require.NoError(t, st.CreateMonitor(ctx, monitor))
	t.Cleanup(func() { _ = st.DeleteMonitor(context.Background(), monitor.ID) })
	rule := &store.Rule{
		MonitorID: monitor.ID, Type: rules.TypeEventEmitted,
		Params: []byte(`{"event_name":"transfer"}`), Enabled: true,
	}
	require.NoError(t, st.CreateRule(ctx, rule))
	require.NoError(t, st.SetIngestState(ctx, network, store.IngestState{LastLedger: 100}))
	require.NoError(t, st.InitializeContractCursor(ctx, network, contractA, 100))
	require.NoError(t, st.InitializeContractCursor(ctx, network, contractB, 100))

	eventA := &stellar.DecodedEvent{
		ID: "postgres-event-a", ContractID: contractA, Ledger: 105,
		Topics: []any{"transfer"}, Value: "injected durable-write failure",
	}
	eventB := &stellar.DecodedEvent{
		ID: "postgres-event-b", ContractID: contractB, Ledger: 105,
		Topics: []any{"transfer"}, Value: "durable",
	}
	source := &contractCursorSource{latest: 120, events: map[string][]*stellar.DecodedEvent{
		contractA: {eventA}, contractB: {eventB},
	}}
	makePoller := func(failEvent string) *Poller {
		failingStore := postgresFailingAlertStore{Store: st, failEvent: failEvent}
		return New(source, failingStore, rules.NewRegistry(), &fakeDispatcher{}, 0, slog.New(slog.DiscardHandler)).WithNetwork(network)
	}
	require.NoError(t, makePoller("postgres-event-a").Poll(ctx))

	cursors, err := st.ListContractCursors(ctx, network)
	require.NoError(t, err)
	positions := make(map[string]uint32, len(cursors))
	for _, cursor := range cursors {
		positions[cursor.ContractID] = cursor.LastLedger
	}
	require.Equal(t, uint32(104), positions[contractA], "failed durable processing leaves A before the event ledger")
	require.Equal(t, uint32(120), positions[contractB], "B commits independently")

	startsBeforeRetry := len(source.starts)
	require.NoError(t, makePoller("").Poll(ctx))
	require.Equal(t, uint32(105), source.starts[startsBeforeRetry], "restart replays A from the failed ledger")
	require.Equal(t, []string{contractA}, source.watch[startsBeforeRetry], "B is not coupled to A's retry")
	cursors, err = st.ListContractCursors(ctx, network)
	require.NoError(t, err)
	for _, cursor := range cursors {
		if cursor.ContractID == contractA {
			require.Equal(t, uint32(120), cursor.LastLedger)
		}
	}
}
