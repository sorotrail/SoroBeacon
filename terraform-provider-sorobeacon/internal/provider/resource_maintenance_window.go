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
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = (*MaintenanceWindowResource)(nil)
var _ resource.ResourceWithImportState = (*MaintenanceWindowResource)(nil)

type MaintenanceWindowResource struct {
	client *apiClient
}

type MaintenanceWindowResourceModel struct {
	ID         types.Int64  `tfsdk:"id"`
	Reason     types.String `tfsdk:"reason"`
	Scope      types.String `tfsdk:"scope"`
	MonitorID  types.Int64  `tfsdk:"monitor_id"`
	ContractID types.String `tfsdk:"contract_id"`
	StartAt    types.String `tfsdk:"start_at"`
	EndAt      types.String `tfsdk:"end_at"`
}

func NewMaintenanceWindowResource() resource.Resource {
	return &MaintenanceWindowResource{}
}

func (r *MaintenanceWindowResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_maintenance_window"
}

func (r *MaintenanceWindowResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a SoroBeacon maintenance window silencing alert deliveries during planned maintenance.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				Computed: true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"reason": schema.StringAttribute{
				Required:    true,
				Description: "Human-readable reason for the maintenance window.",
			},
			"scope": schema.StringAttribute{
				Required:    true,
				Description: "Maintenance scope: 'global', 'monitor', or 'contract'.",
			},
			"monitor_id": schema.Int64Attribute{
				Optional:    true,
				Description: "Target monitor ID when scope is 'monitor'.",
			},
			"contract_id": schema.StringAttribute{
				Optional:    true,
				Description: "Target Soroban contract ID when scope is 'contract'.",
			},
			"start_at": schema.StringAttribute{
				Required:    true,
				Description: "Window start timestamp in RFC3339 format (UTC).",
			},
			"end_at": schema.StringAttribute{
				Required:    true,
				Description: "Window end timestamp in RFC3339 format (UTC); must be after start_at.",
			},
		},
	}
}

func (r *MaintenanceWindowResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*apiClient)
}

func buildMaintenanceWindowBody(plan MaintenanceWindowResourceModel) map[string]any {
	body := map[string]any{
		"reason":   plan.Reason.ValueString(),
		"scope":    plan.Scope.ValueString(),
		"start_at": plan.StartAt.ValueString(),
		"end_at":   plan.EndAt.ValueString(),
	}
	if !plan.MonitorID.IsNull() && !plan.MonitorID.IsUnknown() {
		body["monitor_id"] = plan.MonitorID.ValueInt64()
	} else {
		body["monitor_id"] = nil
	}
	if !plan.ContractID.IsNull() && !plan.ContractID.IsUnknown() && plan.ContractID.ValueString() != "" {
		body["contract_id"] = plan.ContractID.ValueString()
	} else {
		body["contract_id"] = nil
	}
	return body
}

func populateMaintenanceWindowState(result apiMaintenanceWindow, state *MaintenanceWindowResourceModel) {
	state.ID = types.Int64Value(result.ID)
	state.Reason = types.StringValue(result.Reason)
	state.Scope = types.StringValue(result.Scope)
	if result.MonitorID != nil && *result.MonitorID != 0 {
		state.MonitorID = types.Int64Value(*result.MonitorID)
	} else {
		state.MonitorID = types.Int64Null()
	}
	if result.ContractID != nil && *result.ContractID != "" {
		state.ContractID = types.StringValue(*result.ContractID)
	} else {
		state.ContractID = types.StringNull()
	}
	state.StartAt = types.StringValue(result.StartAt.UTC().Format(time.RFC3339))
	state.EndAt = types.StringValue(result.EndAt.UTC().Format(time.RFC3339))
}

func (r *MaintenanceWindowResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan MaintenanceWindowResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result apiMaintenanceWindow
	if err := r.client.create(ctx, "/maintenance", buildMaintenanceWindowBody(plan), &result); err != nil {
		resp.Diagnostics.AddError("Create maintenance window failed", err.Error())
		return
	}

	populateMaintenanceWindowState(result, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MaintenanceWindowResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state MaintenanceWindowResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result apiMaintenanceWindow
	apiPath := fmt.Sprintf("/maintenance/%d", state.ID.ValueInt64())
	if err := r.client.get(ctx, apiPath, &result); err != nil {
		if IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Read maintenance window failed", err.Error())
		return
	}

	populateMaintenanceWindowState(result, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *MaintenanceWindowResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan MaintenanceWindowResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result apiMaintenanceWindow
	apiPath := fmt.Sprintf("/maintenance/%d", plan.ID.ValueInt64())
	if err := r.client.update(ctx, apiPath, buildMaintenanceWindowBody(plan), &result); err != nil {
		resp.Diagnostics.AddError("Update maintenance window failed", err.Error())
		return
	}

	populateMaintenanceWindowState(result, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MaintenanceWindowResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state MaintenanceWindowResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := fmt.Sprintf("/maintenance/%d", state.ID.ValueInt64())
	if err := r.client.delete(ctx, apiPath); err != nil && !IsNotFound(err) {
		resp.Diagnostics.AddError("Delete maintenance window failed", err.Error())
		return
	}
}

func (r *MaintenanceWindowResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := strconv.ParseInt(req.ID, 10, 64)
	if err != nil || id <= 0 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected a positive numeric maintenance window ID")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), types.Int64Value(id))...)
}

type apiMaintenanceWindow struct {
	ID         int64     `json:"id"`
	Reason     string    `json:"reason"`
	Scope      string    `json:"scope"`
	MonitorID  *int64    `json:"monitor_id"`
	ContractID *string   `json:"contract_id"`
	StartAt    time.Time `json:"start_at"`
	EndAt      time.Time `json:"end_at"`
}
