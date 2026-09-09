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
		})
	}
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
