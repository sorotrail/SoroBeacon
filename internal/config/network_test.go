package config

import (
	"testing"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func TestParseNetwork(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantName   string
		wantRPC    string
		wantPhrase string
		wantRPCs   []string // nil means "the single wantRPC entry"
		wantErr    string
	}{
		{
			name:       "unset defaults to testnet",
			env:        map[string]string{},
			wantName:   "testnet",
			wantRPC:    "https://soroban-testnet.stellar.org",
			wantPhrase: stellar.PassphraseTestnet,
		},
		{
			name:       "explicit testnet",
			env:        map[string]string{"NETWORK": "testnet"},
			wantName:   "testnet",
			wantRPC:    "https://soroban-testnet.stellar.org",
			wantPhrase: stellar.PassphraseTestnet,
		},
		{
			name:       "mainnet preset",
			env:        map[string]string{"NETWORK": "mainnet"},
			wantName:   "mainnet",
			wantRPC:    "https://soroban-mainnet.stellar.org",
			wantPhrase: stellar.PassphraseMainnet,
		},
		{
			name:       "futurenet preset",
			env:        map[string]string{"NETWORK": "futurenet"},
			wantName:   "futurenet",
			wantRPC:    "https://rpc-futurenet.stellar.org",
			wantPhrase: stellar.PassphraseFuturenet,
		},
		{
			name:     "custom rpc override keeps preset passphrase",
			env:      map[string]string{"NETWORK": "testnet", "RPC_URL": "http://localhost:8000"},
			wantName: "testnet", wantRPC: "http://localhost:8000", wantPhrase: stellar.PassphraseTestnet,
		},
		{
			name:     "custom passphrase override",
			env:      map[string]string{"NETWORK": "testnet", "NETWORK_PASSPHRASE": "Standalone Network ; February 7th 1974 at 8:37:19 PM"},
			wantName: "testnet", wantRPC: "https://soroban-testnet.stellar.org",
			wantPhrase: "Standalone Network ; February 7th 1974 at 8:37:19 PM",
		},
		{
			name:       "custom network with both set",
			env:        map[string]string{"NETWORK": "custom", "RPC_URL": "http://localhost:8000", "NETWORK_PASSPHRASE": "Standalone Network ; February 7th 1974 at 8:37:19 PM"},
			wantName:   "custom",
			wantRPC:    "http://localhost:8000",
			wantPhrase: "Standalone Network ; February 7th 1974 at 8:37:19 PM",
		},
		{
			name:     "rpc urls list",
			env:      map[string]string{"NETWORK": "testnet", "RPC_URLS": "https://a.example, https://b.example"},
			wantName: "testnet", wantRPC: "https://a.example", wantPhrase: stellar.PassphraseTestnet,
			wantRPCs: []string{"https://a.example", "https://b.example"},
		},
		{
			name:     "rpc urls wins over rpc url",
			env:      map[string]string{"NETWORK": "testnet", "RPC_URL": "https://single.example", "RPC_URLS": "https://a.example,https://b.example"},
			wantName: "testnet", wantRPC: "https://a.example", wantPhrase: stellar.PassphraseTestnet,
			wantRPCs: []string{"https://a.example", "https://b.example"},
		},
		{
			name:     "rpc urls satisfies a custom network without rpc url",
			env:      map[string]string{"NETWORK": "custom", "NETWORK_PASSPHRASE": "p", "RPC_URLS": "https://a.example"},
			wantName: "custom", wantRPC: "https://a.example", wantPhrase: "p",
			wantRPCs: []string{"https://a.example"},
		},
		{
			name:    "rpc urls entry with no scheme",
			env:     map[string]string{"NETWORK": "testnet", "RPC_URLS": "https://a.example,b.example"},
			wantErr: "RPC_URLS",
		},
		{
			name:    "custom without rpc url",
			env:     map[string]string{"NETWORK": "custom", "NETWORK_PASSPHRASE": "p"},
			wantErr: "RPC_URL is required",
		},
		{
			name:    "custom without passphrase",
			env:     map[string]string{"NETWORK": "custom", "RPC_URL": "http://localhost:8000"},
			wantErr: "NETWORK_PASSPHRASE is required",
		},
		{
			name:    "unknown network",
			env:     map[string]string{"NETWORK": "regtest"},
			wantErr: "invalid NETWORK",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseNetwork(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Name != tt.wantName || got.RPCURL != tt.wantRPC || got.Passphrase != tt.wantPhrase {
				t.Fatalf("got %+v, want name=%s rpc=%s phrase=%s", got, tt.wantName, tt.wantRPC, tt.wantPhrase)
			}
			// RPC_URL on its own must keep producing exactly one endpoint.
			wantRPCs := tt.wantRPCs
			if wantRPCs == nil {
				wantRPCs = []string{tt.wantRPC}
			}
			if !slicesEqual(got.RPCURLs, wantRPCs) {
				t.Fatalf("got endpoints %v, want %v", got.RPCURLs, wantRPCs)
			}
		})
	}
}

func TestParseRPCURLs(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr string
	}{
		{name: "unset yields no endpoints", raw: ""},
		{name: "single endpoint", raw: "https://rpc.example", want: []string{"https://rpc.example"}},
		{
			name: "ordered list",
			raw:  "https://primary.example,https://fallback.example",
			want: []string{"https://primary.example", "https://fallback.example"},
		},
		{
			name: "whitespace around entries is trimmed",
			raw:  "  https://primary.example ,  https://fallback.example  ",
			want: []string{"https://primary.example", "https://fallback.example"},
		},
		{
			name: "blank entries are skipped",
			raw:  "https://primary.example,,",
			want: []string{"https://primary.example"},
		},
		{name: "only separators is an error", raw: ",,", wantErr: "no endpoints"},
		{name: "relative url is an error", raw: "rpc.example", wantErr: "https"},
		{name: "unsupported scheme is an error", raw: "ftp://rpc.example", wantErr: "https"},
		{name: "a later entry is still validated", raw: "https://ok.example,not-a-url", wantErr: "not-a-url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRPCURLs(tt.raw)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slicesEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// slicesEqual compares two string slices, treating nil and empty as equal so a
// test can say "no endpoints" either way.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestVerifyPassphrase(t *testing.T) {
	if err := VerifyPassphrase(stellar.PassphraseTestnet, stellar.PassphraseTestnet); err != nil {
		t.Fatalf("matching passphrases should verify, got %v", err)
	}
	if err := VerifyPassphrase("", ""); err != nil {
		t.Fatalf("empty passphrases skip verification, got %v", err)
	}
	err := VerifyPassphrase(stellar.PassphraseMainnet, stellar.PassphraseTestnet)
	if err == nil {
		t.Fatal("mainnet config against testnet node must fail")
	}
	if !contains(err.Error(), "network mismatch") || !contains(err.Error(), "mainnet") || !contains(err.Error(), "testnet") {
		t.Fatalf("error should name both networks, got %q", err.Error())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || stringIndex(s, sub) >= 0)
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestParseNetworks pins the multi-network list syntax: which variables belong
// to which chain, and the two mistakes that would silently point a chain's
// monitors at the wrong ledger stream (a duplicated entry, and a NETWORK whose
// RPC_URL is inherited by a different primary).
func TestParseNetworks(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    []Network // nil when wantErr is set
		wantErr string
	}{
		{
			// The case every deployment is in today: unset means exactly
			// what ParseNetwork resolves, so no caller branches on it.
			name: "unset is the single network",
			env:  map[string]string{"NETWORK": "mainnet"},
			want: []Network{{Name: "mainnet", Passphrase: stellar.PassphraseMainnet,
				RPCURL: "https://soroban-mainnet.stellar.org", RPCURLs: []string{"https://soroban-mainnet.stellar.org"}}},
		},
		{
			name: "primary keeps the unsuffixed variables",
			env:  map[string]string{"NETWORKS": "testnet,mainnet"},
			want: []Network{
				{Name: "testnet", Passphrase: stellar.PassphraseTestnet, RPCURL: "https://soroban-testnet.stellar.org", RPCURLs: []string{"https://soroban-testnet.stellar.org"}},
				{Name: "mainnet", Passphrase: stellar.PassphraseMainnet, RPCURL: "https://soroban-mainnet.stellar.org", RPCURLs: []string{"https://soroban-mainnet.stellar.org"}},
			},
		},
		{
			name: "the list order is the primary",
			env:  map[string]string{"NETWORKS": "mainnet,testnet", "RPC_URL": "http://localhost:8000"},
			want: []Network{
				{Name: "mainnet", Passphrase: stellar.PassphraseMainnet, RPCURL: "http://localhost:8000", RPCURLs: []string{"http://localhost:8000"}},
				{Name: "testnet", Passphrase: stellar.PassphraseTestnet, RPCURL: "https://soroban-testnet.stellar.org", RPCURLs: []string{"https://soroban-testnet.stellar.org"}},
			},
		},
		{
			// Non-primary chains read suffixed names only: an unsuffixed
			// RPC_URL_MAINNET would be read by the primary too, and two
			// chains sharing an endpoint is the corruption this prevents.
			name: "suffixed variables per chain",
			env: map[string]string{
				"NETWORKS":                   "testnet,mainnet",
				"RPC_URL_MAINNET":            "https://mainnet.example",
				"NETWORK_PASSPHRASE_MAINNET": "Custom Main ; Zero",
				"RPC_URLS_TESTNET_UNRELATED": "https://ignored",
			},
			want: []Network{
				{Name: "testnet", Passphrase: stellar.PassphraseTestnet, RPCURL: "https://soroban-testnet.stellar.org", RPCURLs: []string{"https://soroban-testnet.stellar.org"}},
				{Name: "mainnet", Passphrase: "Custom Main ; Zero", RPCURL: "https://mainnet.example", RPCURLs: []string{"https://mainnet.example"}},
			},
		},
		{
			name: "suffixed RPC_URLS fails over within its own chain",
			env: map[string]string{
				"NETWORKS":         "testnet,mainnet",
				"RPC_URLS_MAINNET": "https://a.example,https://b.example",
			},
			want: []Network{
				{Name: "testnet", Passphrase: stellar.PassphraseTestnet, RPCURL: "https://soroban-testnet.stellar.org", RPCURLs: []string{"https://soroban-testnet.stellar.org"}},
				{Name: "mainnet", Passphrase: stellar.PassphraseMainnet, RPCURL: "https://a.example", RPCURLs: []string{"https://a.example", "https://b.example"}},
			},
		},
		{
			name: "stray commas and spacing are trimmed",
			env:  map[string]string{"NETWORKS": " testnet , MAINNET ,"},
			want: []Network{
				{Name: "testnet", Passphrase: stellar.PassphraseTestnet, RPCURL: "https://soroban-testnet.stellar.org", RPCURLs: []string{"https://soroban-testnet.stellar.org"}},
				{Name: "mainnet", Passphrase: stellar.PassphraseMainnet, RPCURL: "https://soroban-mainnet.stellar.org", RPCURLs: []string{"https://soroban-mainnet.stellar.org"}},
			},
		},
		{
			name:    "a chain twice would poll it twice",
			env:     map[string]string{"NETWORKS": "testnet,mainnet,testnet"},
			wantErr: `"testnet" listed twice`,
		},
		{
			name:    "set but empty is not the same as unset",
			env:     map[string]string{"NETWORKS": " , "},
			wantErr: "invalid NETWORKS: set but contains no network names",
		},
		{
			name:    "NETWORK must be the primary",
			env:     map[string]string{"NETWORK": "mainnet", "NETWORKS": "testnet,mainnet"},
			wantErr: `NETWORKS must start with NETWORK="mainnet"`,
		},
		{
			name: "NETWORK agreeing with the primary is fine",
			env:  map[string]string{"NETWORK": "testnet", "NETWORKS": "testnet,mainnet"},
			want: []Network{
				{Name: "testnet", Passphrase: stellar.PassphraseTestnet, RPCURL: "https://soroban-testnet.stellar.org", RPCURLs: []string{"https://soroban-testnet.stellar.org"}},
				{Name: "mainnet", Passphrase: stellar.PassphraseMainnet, RPCURL: "https://soroban-mainnet.stellar.org", RPCURLs: []string{"https://soroban-mainnet.stellar.org"}},
			},
		},
		{
			name:    "unknown entries are reported against NETWORKS",
			env:     map[string]string{"NETWORKS": "testnet,regtest"},
			wantErr: `invalid NETWORKS entry "regtest"`,
		},
		{
			// A private chain has no preset to fall back on, and the
			// requirement is per chain: testnet's endpoint must not be
			// mistaken for custom's.
			name:    "custom needs its own suffixed endpoint",
			env:     map[string]string{"NETWORKS": "testnet,custom"},
			wantErr: "RPC_URL_CUSTOM is required when NETWORKS includes custom",
		},
		{
			name: "custom with both suffixed values",
			env: map[string]string{
				"NETWORKS":                  "testnet,custom",
				"RPC_URL_CUSTOM":            "http://localhost:8000",
				"NETWORK_PASSPHRASE_CUSTOM": "Standalone Network ; February 7th 1974 at 8:37:19 PM",
			},
			want: []Network{
				{Name: "testnet", Passphrase: stellar.PassphraseTestnet, RPCURL: "https://soroban-testnet.stellar.org", RPCURLs: []string{"https://soroban-testnet.stellar.org"}},
				{Name: "custom", Passphrase: "Standalone Network ; February 7th 1974 at 8:37:19 PM", RPCURL: "http://localhost:8000", RPCURLs: []string{"http://localhost:8000"}},
			},
		},
		{
			name:    "a bad suffixed endpoint is caught at startup",
			env:     map[string]string{"NETWORKS": "testnet,mainnet", "RPC_URL_MAINNET": "not-a-url"},
			wantErr: "RPC_URL_MAINNET",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			got, err := ParseNetworks(getenv)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got %+v", tt.wantErr, got)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d networks %v, want %d", len(got), NetworkNames(got), len(tt.want))
			}
			for i := range got {
				if got[i].Name != tt.want[i].Name || got[i].Passphrase != tt.want[i].Passphrase || got[i].RPCURL != tt.want[i].RPCURL {
					t.Fatalf("network[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
				if !slicesEqual(got[i].RPCURLs, tt.want[i].RPCURLs) {
					t.Fatalf("network[%d] endpoints %v, want %v", i, got[i].RPCURLs, tt.want[i].RPCURLs)
				}
			}
		})
	}
}

// TestNetworkNamesIsNamesOnly pins the logging helper: passing a []Network to a
// logger would put a passphrase in the log line.
func TestNetworkNamesIsNamesOnly(t *testing.T) {
	nets := []Network{{Name: "testnet", Passphrase: "secret phrase"}, {Name: "mainnet", Passphrase: "other phrase"}}
	got := NetworkNames(nets)
	if !slicesEqual(got, []string{"testnet", "mainnet"}) {
		t.Fatalf("got %v", got)
	}
	for _, s := range got {
		if contains(s, "phrase") {
			t.Fatalf("passphrase leaked into a name: %q", s)
		}
	}
}
