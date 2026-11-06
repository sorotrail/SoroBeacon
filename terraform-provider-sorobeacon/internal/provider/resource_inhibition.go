package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = (*InhibitionResource)(nil)
var _ resource.ResourceWithImportState = (*InhibitionResource)(nil)

type InhibitionResource struct {
	client *apiClient
}

type InhibitionResourceModel struct {
	ID                  types.String `tfsdk:"id"`
	SourceRuleID        types.Int64  `tfsdk:"source_rule_id"`
	TargetRuleID        types.Int64  `tfsdk:"target_rule_id"`
	FiringWindowSeconds types.Int64  `tfsdk:"firing_window_seconds"`
}

func NewInhibitionResource() resource.Resource {
	return &InhibitionResource{}
}

func (r *InhibitionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_inhibition"
}

func (r *InhibitionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages an alert inhibition relationship between two rules (`POST /inhibitions`).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Composite identifier `<source_rule_id>/<target_rule_id>`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"source_rule_id": schema.Int64Attribute{
				Required:    true,
				Description: "Rule ID whose recent firing suppresses alerts from target_rule_id. Changing this forces replacement.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"target_rule_id": schema.Int64Attribute{
				Required:    true,
				Description: "Rule ID suppressed while source_rule_id is firing. Changing this forces replacement.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"firing_window_seconds": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(600),
				Description: "Lookback window in seconds within which a source rule firing suppresses the target rule. Changing this forces replacement.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
		},
	}
}

func (r *InhibitionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*apiClient)
}

func formatInhibitionID(sourceID, targetID int64) string {
	return fmt.Sprintf("%d/%d", sourceID, targetID)
}

func (r *InhibitionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan InhibitionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := map[string]any{
		"source_rule_id":        plan.SourceRuleID.ValueInt64(),
		"target_rule_id":        plan.TargetRuleID.ValueInt64(),
		"firing_window_seconds": plan.FiringWindowSeconds.ValueInt64(),
	}

	var result apiInhibition
	if err := r.client.create(ctx, "/inhibitions", body, &result); err != nil {
		resp.Diagnostics.AddError("Create inhibition failed", err.Error())
		return
	}

	plan.ID = types.StringValue(formatInhibitionID(result.SourceRuleID, result.TargetRuleID))
	plan.SourceRuleID = types.Int64Value(result.SourceRuleID)
	plan.TargetRuleID = types.Int64Value(result.TargetRuleID)
	plan.FiringWindowSeconds = types.Int64Value(result.FiringWindowSeconds)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *InhibitionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state InhibitionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var listResp struct {
		Inhibitions []apiInhibition `json:"inhibitions"`
	}
	if err := r.client.get(ctx, "/inhibitions", &listResp); err != nil {
		if IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Read inhibitions failed", err.Error())
		return
	}

	for _, inh := range listResp.Inhibitions {
		if inh.SourceRuleID == state.SourceRuleID.ValueInt64() && inh.TargetRuleID == state.TargetRuleID.ValueInt64() {
			state.ID = types.StringValue(formatInhibitionID(inh.SourceRuleID, inh.TargetRuleID))
			state.FiringWindowSeconds = types.Int64Value(inh.FiringWindowSeconds)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *InhibitionResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// All attributes carry RequiresReplace, so Terraform replaces rather than updates in-place.
	resp.Diagnostics.AddError("Inhibition update not supported", "Inhibition attributes require replacement")
}

func (r *InhibitionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state InhibitionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := fmt.Sprintf("/inhibitions/%d/%d", state.SourceRuleID.ValueInt64(), state.TargetRuleID.ValueInt64())
	if err := r.client.delete(ctx, apiPath); err != nil && !IsNotFound(err) {
		resp.Diagnostics.AddError("Delete inhibition failed", err.Error())
		return
	}
}

func (r *InhibitionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected format: source_rule_id/target_rule_id")
		return
	}
	sourceID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || sourceID <= 0 {
		resp.Diagnostics.AddError("Invalid source_rule_id", "Expected a positive integer")
		return
	}
	targetID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || targetID <= 0 {
		resp.Diagnostics.AddError("Invalid target_rule_id", "Expected a positive integer")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), types.StringValue(formatInhibitionID(sourceID, targetID)))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("source_rule_id"), types.Int64Value(sourceID))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("target_rule_id"), types.Int64Value(targetID))...)
}

type apiInhibition struct {
	SourceRuleID        int64 `json:"source_rule_id"`
	TargetRuleID        int64 `json:"target_rule_id"`
	FiringWindowSeconds int64 `json:"firing_window_seconds"`
}
