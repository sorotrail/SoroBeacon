package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func TestNetworkByNameHappyPath(t *testing.T) {
	t.Helper()
	net := NetworkByName("testnet")
	require.NotNil(t, net)
	require.Equal(t, "testnet", net.Name)
	require.Equal(t, stellar.PassphraseTestnet, net.Passphrase)
}

func TestNetworkByNameErrorPath(t *testing.T) {
	t.Helper()
	net := NetworkByName("unknown-net")
	require.Nil(t, net)
}