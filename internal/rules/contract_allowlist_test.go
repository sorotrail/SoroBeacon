package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

const (
	allowTreasury = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
	allowOther    = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

func allowlistEvent(contractID string) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000000",
		ContractID: contractID,
		Topics:     []any{"transfer"},
	}
}

func TestContractAllowlistEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		event   *stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		{
			name:   "in-list contract matches",
			params: `{"contract_ids": ["` + allowTreasury + `", "` + allowOther + `"]}`,
			event:  allowlistEvent(allowTreasury),
			want:   true,
		},
		{
			name:   "contract not in list does not match",
			params: `{"contract_ids": ["` + allowTreasury + `"]}`,
			event:  allowlistEvent(allowOther),
			want:   false,
		},
		{
			name:   "exclude inverts a member into a non-match",
			params: `{"contract_ids": ["` + allowTreasury + `"], "exclude": true}`,
			event:  allowlistEvent(allowTreasury),
			want:   false,
		},
		{
			name:   "exclude inverts a non-member into a match",
			params: `{"contract_ids": ["` + allowTreasury + `"], "exclude": true}`,
			event:  allowlistEvent(allowOther),
			want:   true,
		},
		{
			name:   "exclude defaults to false",
			params: `{"contract_ids": ["` + allowTreasury + `"]}`,
			event:  allowlistEvent(allowOther),
			want:   false,
		},
		{
			name:    "invalid contract ID errors",
			params:  `{"contract_ids": ["NOTACONTRACT"]}`,
			event:   allowlistEvent(allowTreasury),
			wantErr: true,
		},
		{
			name:    "empty list errors",
			params:  `{"contract_ids": []}`,
			event:   allowlistEvent(allowTreasury),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (&ContractAllowlist{}).Evaluate(context.Background(), tt.event, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestContractAllowlistValidate(t *testing.T) {
	v := &ContractAllowlist{}
	assert.NoError(t, v.Validate(json.RawMessage(`{"contract_ids": ["`+allowTreasury+`"]}`)))
	assert.NoError(t, v.Validate(json.RawMessage(`{"contract_ids": ["`+allowTreasury+`", "`+allowOther+`"], "exclude": true}`)))

	assert.Error(t, v.Validate(json.RawMessage(`{"contract_ids": []}`)), "empty list rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{}`)), "missing list rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{"contract_ids": ["NOTACONTRACT"]}`)), "invalid contract ID rejected")
	assert.Error(t, v.Validate(json.RawMessage(`{"contract_ids": [""]}`)), "empty ID rejected")
}

func TestContractAllowlistRegistered(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Validate(TypeContractAllowlist, json.RawMessage(`{"contract_ids": ["`+allowTreasury+`"]}`)))
	got, err := r.Evaluate(context.Background(), TypeContractAllowlist, allowlistEvent(allowTreasury), json.RawMessage(`{"contract_ids": ["`+allowTreasury+`"]}`))
	require.NoError(t, err)
	assert.True(t, got)
}
