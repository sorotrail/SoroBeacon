package workspace

import (
	"errors"
	"testing"
)

func TestValidateDeletionConfirmation(t *testing.T) {
	tests := []struct {
		name    string
		target  ID
		confirm string
		want    error
	}{
		{name: "confirmation matches the workspace id", target: "acme", confirm: "acme", want: nil},
		{name: "missing confirmation", target: "acme", confirm: "", want: ErrDeletionConfirmationRequired},
		{name: "confirmation names another workspace", target: "acme", confirm: "acme-eu", want: ErrDeletionConfirmationRequired},
		{name: "confirmation differs only in case", target: "acme", confirm: "ACME", want: ErrDeletionConfirmationRequired},
		{name: "confirmation with surrounding space", target: "acme", confirm: " acme ", want: ErrDeletionConfirmationRequired},
		{name: "the default workspace cannot be deleted", target: Default, confirm: string(Default), want: ErrCannotDeleteDefaultWorkspace},
		{name: "invalid target id", target: "Not A Slug!", confirm: "Not A Slug!", want: ErrInvalidID},
		{name: "empty target id", target: "", confirm: "", want: ErrInvalidID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDeletionConfirmation(tc.target, tc.confirm)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateDeletionConfirmation(%q, %q) = %v, want %v", tc.target, tc.confirm, err, tc.want)
			}
		})
	}
}
