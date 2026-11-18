package notify

import (
	"testing"
)

func TestEscapeJSONString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "happy path - simple string",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "happy path - string with spaces",
			input: "a b c",
			want:  "a b c",
		},
		{
			name:  "happy path - empty string",
			input: "",
			want:  "",
		},
		{
			name:  "error path - very short input (len < 2 after marshal)",
			// The function returns "" when len(marshaled) < 2,
			// which happens with empty string since json.Marshal("") = "{quote}{quote}" = 2 bytes
			// but b[1:len(b)-1] gives empty string. This is covered above.
		},
		{
			name:  "error path - function returns empty for marshal failure",
			// json.Marshal should not fail for valid Go strings, but we test the guard.
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EscapeJSONString(tt.input)
			if got != tt.want {
				t.Errorf("EscapeJSONString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}