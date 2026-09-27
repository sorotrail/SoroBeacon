package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = (*RuleResource)(nil)
var _ resource.ResourceWithImportState = (*RuleResource)(nil)

type RuleResource struct {
	client *apiClient
}

type RuleResourceModel struct {
	ID        types.Int64  `tfsdk:"id"`
	MonitorID types.Int64  `tfsdk:"monitor_id"`
	Type      types.String `tfsdk:"type"`
	Params    types.String `tfsdk:"params"`
	Enabled   types.Bool   `tfsdk:"enabled"`
}

func NewRuleResource() resource.Resource {
	return &RuleResource{}
}

func (r *RuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_rule"
}

func (r *RuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a rule on a SoroBeacon monitor.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				Computed: true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"monitor_id": schema.Int64Attribute{
				Required:    true,
				Description: "Parent monitor ID.",
			},
			"type": schema.StringAttribute{
				Required:    true,
				Description: "Rule type (event_emitted, value_threshold, etc.).",
			},
			"params": schema.StringAttribute{
				Required:    true,
				Description: "Rule parameters as a JSON string.",
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
		},
	}
}

func (r *RuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*apiClient)
}

func (r *RuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan RuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var params json.RawMessage
	if err := json.Unmarshal([]byte(plan.Params.ValueString()), &params); err != nil {
		resp.Diagnostics.AddError("Invalid params JSON", err.Error())
		return
	}

	body := map[string]any{
		"type":    plan.Type.ValueString(),
		"params":  params,
		"enabled": plan.Enabled.ValueBool(),
	}

	var result apiRule
	path := fmt.Sprintf("/monitors/%d/rules", plan.MonitorID.ValueInt64())
	if err := r.client.create(ctx, path, body, &result); err != nil {
		resp.Diagnostics.AddError("Create rule failed", err.Error())
		return
	}

	plan.ID = types.Int64Value(result.ID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *RuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state RuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var rules []apiRule
	path := fmt.Sprintf("/monitors/%d/rules", state.MonitorID.ValueInt64())
	if err := r.client.get(ctx, path, &rules); err != nil {
		resp.Diagnostics.AddError("Read rules failed", err.Error())
		return
	}

	for _, rule := range rules {
		if rule.ID == state.ID.ValueInt64() {
			state.Type = types.StringValue(rule.Type)
			state.Params = types.StringValue(string(rule.Params))
			state.Enabled = types.BoolValue(rule.Enabled)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *RuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan RuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var params json.RawMessage
	if err := json.Unmarshal([]byte(plan.Params.ValueString()), &params); err != nil {
		resp.Diagnostics.AddError("Invalid params JSON", err.Error())
		return
	}

	body := map[string]any{
		"type":    plan.Type.ValueString(),
		"params":  params,
		"enabled": plan.Enabled.ValueBool(),
	}

	var result apiRule
	path := fmt.Sprintf("/monitors/%d/rules/%d", plan.MonitorID.ValueInt64(), plan.ID.ValueInt64())
	if err := r.client.update(ctx, path, body, &result); err != nil {
		resp.Diagnostics.AddError("Update rule failed", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *RuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state RuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	path := fmt.Sprintf("/monitors/%d/rules/%d", state.MonitorID.ValueInt64(), state.ID.ValueInt64())
	if err := r.client.delete(ctx, path); err != nil {
		resp.Diagnostics.AddError("Delete rule failed", err.Error())
		return
	}
}

func (r *RuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import ID format: monitor_id/rule_id
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected format: monitor_id/rule_id")
		return
	}
	monitorID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid monitor ID", err.Error())
		return
	}
	ruleID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid rule ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("monitor_id"), types.Int64Value(monitorID))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), types.Int64Value(ruleID))...)
}

type apiRule struct {
	ID        int64           `json:"id"`
	MonitorID int64           `json:"monitor_id"`
	Type      string          `json:"type"`
	Params    json.RawMessage `json:"params"`
	Enabled   bool            `json:"enabled"`
}
