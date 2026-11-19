package auth

import "testing"

func TestTokenIsRotating(t *testing.T) {
	successor := int64(42)
	zero := int64(0)
	tests := []struct {
		name  string
		token *Token
		want  bool
	}{
		{name: "token with a successor is rotating", token: &Token{SuccessorID: &successor}, want: true},
		{name: "a set successor id counts even when it is zero", token: &Token{SuccessorID: &zero}, want: true},
		{name: "token without a successor is not rotating", token: &Token{}, want: false},
		{name: "nil token is not rotating", token: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.token.IsRotating(); got != tc.want {
				t.Fatalf("IsRotating() = %v, want %v", got, tc.want)
			}
		})
	}
}
