package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// Network describes one Stellar network: the passphrase that identifies it
// and the RPC endpoint(s) that serve it.
type Network struct {
	// Name is the short label used in config and logs (testnet, mainnet,
	// futurenet, custom).
	Name string
	// Passphrase is the network passphrase — a Stellar network's identity.
	Passphrase string
	// RPCURL is the endpoint calls try first: RPC_URLS[0] when the ordered
	// list is set, RPC_URL (or the preset) otherwise. Kept as a direct
	// field because most call sites only ever need one URL.
	RPCURL string
	// RPCURLs is the ordered endpoint list RPC_URLS resolves to. It is
	// never empty: with only RPC_URL set it holds that single endpoint, so
	// the failover client has exactly one candidate and behaves as before.
	RPCURLs []string
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

// ParseNetwork resolves the NETWORK, RPC_URL, RPC_URLS and
// NETWORK_PASSPHRASE variables into a Network. The rules are:
//
//   - NETWORK unset or "testnet": the testnet defaults, as before.
//   - NETWORK one of mainnet/futurenet: that network's public RPC and
//     passphrase, unless RPC_URL overrides the endpoint.
//   - NETWORK "custom": RPC_URL (or RPC_URLS) and NETWORK_PASSPHRASE must
//     both be set — for private standalone networks with a passphrase of
//     your choosing.
//   - NETWORK_PASSPHRASE always wins over the preset, so a named network
//     with a custom passphrase (e.g. a local quickstart testnet) works.
//   - RPC_URLS, when set, replaces the single endpoint with an ordered list
//     to fail over between; RPC_URL is ignored except as its fallback.
//
// Both spellings are fine on their own. Only when neither is set (and the
// network has no preset) is an endpoint missing.
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

	// RPC_URLS wins over RPC_URL when both are set: an explicit ordered
	// list is the operator saying which endpoints to use and in what order
	// to fail over. RPC_URL never stops working on its own — it just becomes
	// the single-entry case of the same list.
	urls, err := ParseRPCURLs(getenv("RPC_URLS"))
	if err != nil {
		return Network{}, err
	}
	switch {
	case len(urls) > 0:
		net.RPCURLs = urls
		net.RPCURL = urls[0]
	case net.RPCURL != "":
		net.RPCURLs = []string{net.RPCURL}
	}

	if net.RPCURL == "" {
		return Network{}, fmt.Errorf("RPC_URL is required when NETWORK=%s (or set RPC_URLS to a comma-separated list)", name)
	}
	if net.Passphrase == "" {
		return Network{}, fmt.Errorf("NETWORK_PASSPHRASE is required when NETWORK=custom")
	}
	return net, nil
}

// ParseRPCURLs splits RPC_URLS on commas into the ordered endpoint list.
// Entries are trimmed, so "a, b" and "a,b" are the same list, and blank
// entries (a stray or trailing comma) are skipped.
//
// An unset value yields no endpoints and lets the caller fall back to
// RPC_URL. A value that is set but yields nothing usable (a lone comma) is an
// error rather than a silent fallback: the operator clearly meant to list
// endpoints, and quietly polling one they did not name is worse than stopping.
// Every entry must be an absolute http(s) URL, checked here so a typo fails at
// startup instead of on the first poll.
func ParseRPCURLs(raw string) ([]string, error) {
	var urls []string
	for _, u := range strings.Split(raw, ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	if raw != "" && len(urls) == 0 {
		return nil, fmt.Errorf("invalid RPC_URLS: set but contains no endpoints (use comma-separated http(s) URLs, or unset it to use RPC_URL)")
	}
	for _, u := range urls {
		if !validRPCURL(u) {
			return nil, fmt.Errorf("invalid RPC_URLS entry %q: must be an absolute http or https URL", u)
		}
	}
	return urls, nil
}

// validRPCURL reports whether raw is an absolute http or https URL. It backs
// both RPC_URL and RPC_URLS validation so the two cannot drift apart.
func validRPCURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
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
