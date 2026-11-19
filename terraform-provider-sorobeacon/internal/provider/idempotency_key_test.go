package provider

import (
	"context"
	"testing"
)

type otherCtxKey struct{}

func TestIdempotencyKeyFromContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
		want string
	}{
		{
			name: "returns key attached with WithIdempotencyKey",
			ctx:  func() context.Context { return WithIdempotencyKey(context.Background(), "key-123") },
			want: "key-123",
		},
		{
			name: "returns trimmed key",
			ctx:  func() context.Context { return WithIdempotencyKey(context.Background(), "  key-456\t\n") },
			want: "key-456",
		},
		{
			name: "innermost key wins",
			ctx: func() context.Context {
				return WithIdempotencyKey(WithIdempotencyKey(context.Background(), "outer"), "inner")
			},
			want: "inner",
		},
		{
			name: "survives derived contexts",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(WithIdempotencyKey(context.Background(), "key-789"))
				t.Cleanup(cancel)
				return context.WithValue(ctx, otherCtxKey{}, "x")
			},
			want: "key-789",
		},
		{
			name: "no key attached returns empty",
			ctx:  context.Background,
			want: "",
		},
		{
			name: "nil context returns empty",
			ctx:  func() context.Context { return nil },
			want: "",
		},
		{
			name: "unrelated value with same string type is ignored",
			ctx:  func() context.Context { return context.WithValue(context.Background(), otherCtxKey{}, "not-a-key") },
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IdempotencyKeyFromContext(tt.ctx()); got != tt.want {
				t.Fatalf("IdempotencyKeyFromContext() = %q, want %q", got, tt.want)
			}
		})
	}
}
