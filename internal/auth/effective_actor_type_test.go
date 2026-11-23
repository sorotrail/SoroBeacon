package auth

import "testing"

func TestEffectiveActorType(t *testing.T) {
	tests := []struct {
		name      string
		principal *Principal
		want      string
	}{
		{
			name:      "nil principal is anonymous",
			principal: nil,
			want:      "anonymous",
		},
		{
			name:      "explicit actor type is kept",
			principal: &Principal{ActorType: "person"},
			want:      "person",
		},
		{
			name:      "restricted token defaults to automation",
			principal: &Principal{restricted: true},
			want:      "automation",
		},
		{
			name:      "unrestricted caller defaults to person",
			principal: &Principal{},
			want:      "person",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.principal.EffectiveActorType(); got != tt.want {
				t.Errorf("EffectiveActorType() = %q, want %q", got, tt.want)
			}
		})
	}
}
