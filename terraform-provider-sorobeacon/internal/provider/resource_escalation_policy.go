package provider

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = (*EscalationPolicyResource)(nil)
var _ resource.ResourceWithImportState = (*EscalationPolicyResource)(nil)

type EscalationPolicyResource struct {
	client *apiClient
}

type EscalationStepModel struct {
	Delay      types.String `tfsdk:"delay"`
	ChannelIDs types.List   `tfsdk:"channel_ids"`
}

type EscalationPolicyResourceModel struct {
	ID        types.Int64           `tfsdk:"id"`
	MonitorID types.Int64           `tfsdk:"monitor_id"`
	Steps     []EscalationStepModel `tfsdk:"steps"`
}

func NewEscalationPolicyResource() resource.Resource {
	return &EscalationPolicyResource{}
}

func (r *EscalationPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_escalation_policy"
}

func (r *EscalationPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a tiered escalation policy attached to a SoroBeacon monitor (`PUT /monitors/{id}/escalation`).",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				Computed:    true,
				Description: "Escalation policy ID assigned by the server.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"monitor_id": schema.Int64Attribute{
				Required:    true,
				Description: "ID of the monitor this escalation policy belongs to. Changing this forces replacement.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"steps": schema.ListNestedAttribute{
				Required:    true,
				Description: "Ordered list of escalation steps.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"delay": schema.StringAttribute{
							Optional:    true,
							Computed:    true,
							Default:     stringdefault.StaticString("0s"),
							Description: "Go duration string before this step fires (e.g. '0s', '5m').",
						},
						"channel_ids": schema.ListAttribute{
							Required:    true,
							ElementType: types.Int64Type,
							Description: "Notification channel IDs to notify at this step.",
						},
					},
				},
			},
		},
	}
}

func (r *EscalationPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*apiClient)
}

func normalizeDurationString(raw string) string {
	if raw == "" {
		return "0s"
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return (time.Duration(int64(d/time.Second)) * time.Second).String()
	}
	return raw
}

func (r *EscalationPolicyResource) putPolicy(ctx context.Context, plan *EscalationPolicyResourceModel, respDiags interface {
	AddError(string, string)
	HasError() bool
}) (*apiEscalationPolicy, bool) {
	apiSteps := make([]map[string]any, 0, len(plan.Steps))
	for _, st := range plan.Steps {
		var cids []int64
		for _, elem := range st.ChannelIDs.Elements() {
			if iv, ok := elem.(types.Int64); ok && !iv.IsNull() && !iv.IsUnknown() {
				cids = append(cids, iv.ValueInt64())
			}
		}
		delay := st.Delay.ValueString()
		if delay == "" {
			delay = "0s"
		}
		apiSteps = append(apiSteps, map[string]any{
			"delay":       delay,
			"channel_ids": cids,
		})
	}

	var result apiEscalationPolicy
	apiPath := fmt.Sprintf("/monitors/%d/escalation", plan.MonitorID.ValueInt64())
	if err := r.client.put(ctx, apiPath, map[string]any{"steps": apiSteps}, &result); err != nil {
		respDiags.AddError("Save escalation policy failed", err.Error())
		return nil, false
	}
	return &result, true
}

func populateEscalationModelFromAPI(ctx context.Context, result apiEscalationPolicy, state *EscalationPolicyResourceModel) {
	state.ID = types.Int64Value(result.ID)
	state.MonitorID = types.Int64Value(result.MonitorID)
	steps := make([]EscalationStepModel, 0, len(result.Steps))
	for _, st := range result.Steps {
		listVal, _ := types.ListValueFrom(ctx, types.Int64Type, st.ChannelIDs)
		steps = append(steps, EscalationStepModel{
			Delay:      types.StringValue(normalizeDurationString(st.Delay)),
			ChannelIDs: listVal,
		})
	}
	state.Steps = steps
}

func (r *EscalationPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan EscalationPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	result, ok := r.putPolicy(ctx, &plan, &resp.Diagnostics)
	if !ok {
		return
	}
	populateEscalationModelFromAPI(ctx, *result, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *EscalationPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state EscalationPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result apiEscalationPolicy
	apiPath := fmt.Sprintf("/monitors/%d/escalation", state.MonitorID.ValueInt64())
	if err := r.client.get(ctx, apiPath, &result); err != nil {
		if IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Read escalation policy failed", err.Error())
		return
	}

	populateEscalationModelFromAPI(ctx, result, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *EscalationPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan EscalationPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	result, ok := r.putPolicy(ctx, &plan, &resp.Diagnostics)
	if !ok {
		return
	}
	populateEscalationModelFromAPI(ctx, *result, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *EscalationPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state EscalationPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := fmt.Sprintf("/monitors/%d/escalation", state.MonitorID.ValueInt64())
	if err := r.client.delete(ctx, apiPath); err != nil && !IsNotFound(err) {
		resp.Diagnostics.AddError("Delete escalation policy failed", err.Error())
		return
	}
}

func (r *EscalationPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	monitorID, err := strconv.ParseInt(req.ID, 10, 64)
	if err != nil || monitorID <= 0 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected a positive numeric monitor_id")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("monitor_id"), types.Int64Value(monitorID))...)
}

type apiEscalationStep struct {
	Position   int     `json:"position"`
	Delay      string  `json:"delay"`
	ChannelIDs []int64 `json:"channel_ids"`
}

type apiEscalationPolicy struct {
	ID        int64               `json:"id"`
	MonitorID int64               `json:"monitor_id"`
	Steps     []apiEscalationStep `json:"steps"`
}
