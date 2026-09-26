package stellar

import (
	"testing"
)

func TestNetworkName(t *testing.T) {
	tests := []struct {
		name       string
		passphrase string
		want       string
	}{
		{
			name:       "mainnet",
			passphrase: PassphraseMainnet,
			want:       "mainnet",
		},
		{
			name:       "testnet",
			passphrase: PassphraseTestnet,
			want:       "testnet",
		},
		{
			name:       "futurenet",
			passphrase: PassphraseFuturenet,
			want:       "futurenet",
		},
		{
			name:       "unknown",
			passphrase: "some unknown network passphrase",
			want:       "",
		},
		{
			name:       "empty string",
			passphrase: "",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NetworkName(tt.passphrase); got != tt.want {
				t.Errorf("NetworkName() = %v, want %v", got, tt.want)
			}
		})
	}
}
