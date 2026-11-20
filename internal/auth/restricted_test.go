package auth

import "testing"

func TestPrincipalRestricted(t *testing.T) {
	tests := []struct {
		name      string
		principal *Principal
		want      bool
	}{
		{
			name:      "database token is restricted",
			principal: &Principal{restricted: true},
			want:      true,
		},
		{
			name:      "static token or session is unrestricted",
			principal: &Principal{},
			want:      false,
		},
		{
			// No caller is not an unrestricted caller: failing closed keeps a
			// request that never passed the auth middleware from reading as
			// full access.
			name:      "nil principal is not restricted",
			principal: nil,
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.principal.Restricted(); got != tt.want {
				t.Errorf("Restricted() = %v, want %v", got, tt.want)
			}
		})
	}
}
