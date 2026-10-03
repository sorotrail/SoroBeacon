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

// NetworkNames lists the networks' names in order. Logs, metrics labels and
// error messages go through this rather than printing Network values, so a
// passphrase can never end up in a log line by accident.
func NetworkNames(nets []Network) []string {
	names := make([]string, 0, len(nets))
	for _, n := range nets {
		names = append(names, n.Name)
	}
	return names
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
//
// This parses the single-network case, which is also what an instance that
// lists several networks still resolves for its primary; ParseNetworks is the
// entry point the rest of the multi-network code uses.
func ParseNetwork(getenv func(string) string) (Network, error) {
	name := strings.ToLower(strings.TrimSpace(getenv("NETWORK")))
	if name == "" {
		name = "testnet"
	}
	return buildNetwork(getenv, name, "NETWORK", false)
}

// ParseNetworks resolves the list of networks an instance polls, primary first.
//
// NETWORKS is a comma-separated list of network names in the same vocabulary
// NETWORK uses (testnet|mainnet|futurenet|custom). Unset — the case every
// existing deployment is in — this returns exactly one network parsed by
// ParseNetwork, so the single-network configuration keeps working unchanged
// rather than becoming a special case the rest of the code has to handle.
//
// The first entry is the primary: the one RPC_URL, RPC_URLS and
// NETWORK_PASSPHRASE configure. That is what lets NETWORK=mainnet keep meaning
// what it means today while NETWORKS adds chains around it, and it is why
// setting NETWORK *and* a list that starts elsewhere is an error rather than a
// silent re-ordering — RPC_URL pointing at a chain nobody named is the one
// misconfiguration that makes every monitor evaluate the wrong network.
//
// Every later entry takes its own suffixed variables (RPC_URL_MAINNET,
// RPC_URLS_MAINNET, NETWORK_PASSPHRASE_MAINNET) and otherwise uses that
// network's public preset. Suffixed names rather than a `name=url` list syntax
// so one variable holds one value: a paired list would have to be split on both
// commas and equals signs, and an equals sign is legal in a query string, so
// its parse would be the ambiguous kind.
func ParseNetworks(getenv func(string) string) ([]Network, error) {
	raw := strings.TrimSpace(getenv("NETWORKS"))
	if raw == "" {
		net, err := ParseNetwork(getenv)
		if err != nil {
			return nil, err
		}
		return []Network{net}, nil
	}

	var names []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue // a stray or trailing comma is not a network
		}
		if seen[name] {
			return nil, fmt.Errorf("invalid NETWORKS: %q listed twice", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("invalid NETWORKS: set but contains no network names (use comma-separated testnet|mainnet|futurenet|custom names, or unset it to poll only NETWORK)")
	}

	if explicit := strings.ToLower(strings.TrimSpace(getenv("NETWORK"))); explicit != "" && explicit != names[0] {
		return nil, fmt.Errorf(
			"NETWORKS must start with NETWORK=%q: its first entry is the primary network that RPC_URL and NETWORK_PASSPHRASE configure (got %q)",
			explicit, names[0])
	}

	nets := make([]Network, 0, len(names))
	for i, name := range names {
		net, err := buildNetwork(getenv, name, "NETWORKS", i > 0)
		if err != nil {
			return nil, err
		}
		nets = append(nets, net)
	}
	return nets, nil
}

// buildNetwork turns one already-normalised network name into a Network,
// reading that network's endpoint and passphrase overrides.
//
// varLabel names the variable the name came from, so a bad list entry is
// reported against NETWORKS rather than NETWORK. suffixed selects the
// per-network variable names (see ParseNetworks).
func buildNetwork(getenv func(string) string, name, varLabel string, suffixed bool) (Network, error) {
	// varName is the single place that knows which variable belongs to this
	// network, so the checks below and their error messages cannot disagree
	// about the name an operator should set.
	varName := func(key string) string {
		if !suffixed {
			return key
		}
		return key + "_" + strings.ToUpper(name)
	}
	get := func(key string) string { return getenv(varName(key)) }

	var net Network
	switch name {
	case "testnet", "mainnet", "futurenet":
		net = *NetworkByName(name)
	case "custom":
		net = Network{Name: "custom"}
	default:
		return Network{}, fmt.Errorf("invalid %s entry %q (want testnet|mainnet|futurenet|custom)", varLabel, name)
	}
	net.Name = name

	if v := get("RPC_URL"); v != "" {
		if !validRPCURL(v) {
			return Network{}, fmt.Errorf("invalid %s %q: must be an absolute http or https URL", varName("RPC_URL"), v)
		}
		net.RPCURL = v
	}
	if v := get("NETWORK_PASSPHRASE"); v != "" {
		net.Passphrase = v
	}

	// RPC_URLS wins over RPC_URL when both are set: an explicit ordered
	// list is the operator saying which endpoints to use and in what order
	// to fail over. RPC_URL never stops working on its own — it just becomes
	// the single-entry case of the same list.
	urls, err := ParseRPCURLs(get("RPC_URLS"))
	if err != nil {
		return Network{}, fmt.Errorf("%s: %w", varName("RPC_URLS"), err)
	}
	switch {
	case len(urls) > 0:
		net.RPCURLs = urls
		net.RPCURL = urls[0]
	case net.RPCURL != "":
		net.RPCURLs = []string{net.RPCURL}
	}

	if net.RPCURL == "" {
		return Network{}, fmt.Errorf("%s is required when %s includes %s (or set %s to a comma-separated list)",
			varName("RPC_URL"), varLabel, name, varName("RPC_URLS"))
	}
	if net.Passphrase == "" {
		return Network{}, fmt.Errorf("%s is required when %s includes %s", varName("NETWORK_PASSPHRASE"), varLabel, name)
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
