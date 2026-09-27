package provider

import (
	"context"
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func expandStringList(ctx context.Context, list types.List, diags *diag.Diagnostics) []string {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var result []string
	diags.Append(list.ElementsAs(ctx, &result, false)...)
	return result
}

func expandInt64List(ctx context.Context, list types.List, diags *diag.Diagnostics) []int64 {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var result []int64
	diags.Append(list.ElementsAs(ctx, &result, false)...)
	return result
}

// jsonRaw wraps a string as json.RawMessage for API calls so the JSON is
// sent as an object rather than a quoted string.
func jsonRaw(s string) json.RawMessage {
	return json.RawMessage(s)
}
