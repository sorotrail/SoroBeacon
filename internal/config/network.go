package config

import (
	"fmt"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// Network describes one Stellar network: the passphrase that identifies it
// and the public RPC endpoint that serves it.
type Network struct {
	// Name is the short label used in config and logs (testnet, mainnet,
	// futurenet, custom).
	Name string
	// Passphrase is the network passphrase — a Stellar network's identity.
	Passphrase string
	// RPCURL is the public RPC endpoint for this network.
	RPCURL string
}

// KnownNetworks are the SDF-maintained networks with public RPC endpoints.
var KnownNetworks = []Network{
	{Name: "testnet", Passphrase: stellar.PassphraseTestnet, RPCURL: "https://soroban-testnet.stellar.org"},
	{Name: "mainnet", Passphrase: stellar.PassphraseMainnet, RPCURL: "https://soroban-mainnet.stellar.org"},
	{Name: "futurenet", Passphrase: stellar.PassphraseFuturenet, RPCURL: "https://rpc-futurenet.stellar.org"},
}

// NetworkByName returns the known network with the given name, or nil.
func NetworkByName(name string) *Network {
	for i, n := range KnownNetworks {
		if n.Name == name {
			return &KnownNetworks[i]
		}
	}
	return nil
}

// ParseNetwork resolves the NETWORK, RPC_URL and NETWORK_PASSPHRASE
// variables into a Network. The rules are:
//
//   - NETWORK unset or "testnet": the testnet defaults, as before.
//   - NETWORK one of mainnet/futurenet: that network's public RPC and
//     passphrase, unless RPC_URL overrides the endpoint.
//   - NETWORK "custom": RPC_URL and NETWORK_PASSPHRASE must both be set —
//     for private standalone networks with a passphrase of your choosing.
//   - NETWORK_PASSPHRASE always wins over the preset, so a named network
//     with a custom passphrase (e.g. a local quickstart testnet) works.
func ParseNetwork(getenv func(string) string) (Network, error) {
	name := strings.ToLower(strings.TrimSpace(getenv("NETWORK")))
	if name == "" {
		name = "testnet"
	}

	var net Network
	switch name {
	case "testnet", "mainnet", "futurenet":
		preset := NetworkByName(name)
		net = *preset
	case "custom":
		net = Network{Name: "custom"}
	default:
		return Network{}, fmt.Errorf("invalid NETWORK %q (want testnet|mainnet|futurenet|custom)", name)
	}

	if v := getenv("RPC_URL"); v != "" {
		net.RPCURL = v
	}
	if v := getenv("NETWORK_PASSPHRASE"); v != "" {
		net.Passphrase = v
	}

	if net.RPCURL == "" {
		return Network{}, fmt.Errorf("RPC_URL is required when NETWORK=%s", name)
	}
	if net.Passphrase == "" {
		return Network{}, fmt.Errorf("NETWORK_PASSPHRASE is required when NETWORK=custom")
	}
	return net, nil
}

// VerifyPassphrase reports whether the passphrase reported by the connected
// RPC node matches the configured one. A mismatch means the deployment is
// pointed at a different network than it believes — every monitor and rule
// on the instance is evaluating the wrong chain.
func VerifyPassphrase(configured, reported string) error {
	if configured == "" || reported == "" {
		return nil
	}
	if configured != reported {
		label := func(p string) string {
			if n := stellar.NetworkName(p); n != "" {
				return fmt.Sprintf("%s (%q)", n, p)
			}
			return fmt.Sprintf("%q", p)
		}
		return fmt.Errorf(
			"network mismatch: configured for %s but the RPC endpoint belongs to %s — check NETWORK / RPC_URL",
			label(configured), label(reported))
	}
	return nil
}
