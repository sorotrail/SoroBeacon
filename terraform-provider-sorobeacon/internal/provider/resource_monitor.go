package provider

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = (*MonitorResource)(nil)
var _ resource.ResourceWithImportState = (*MonitorResource)(nil)

type MonitorResource struct {
	client *apiClient
}

type MonitorResourceModel struct {
	ID          types.Int64  `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	ContractIDs types.List   `tfsdk:"contract_ids"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	ChannelIDs  types.List   `tfsdk:"channel_ids"`
}

func NewMonitorResource() resource.Resource {
	return &MonitorResource{}
}

func (r *MonitorResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_monitor"
}

func (r *MonitorResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a SoroBeacon monitor.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				Computed: true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Monitor display name.",
			},
			"contract_ids": schema.ListAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Soroban contract addresses to watch.",
			},
			"enabled": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Whether the monitor is active.",
			},
			"channel_ids": schema.ListAttribute{
				Optional:    true,
				ElementType: types.Int64Type,
				Description: "Notification channel IDs to alert.",
			},
		},
	}
}

func (r *MonitorResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*apiClient)
}

func (r *MonitorResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan MonitorResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	contractIDs := expandStringList(ctx, plan.ContractIDs, &resp.Diagnostics)
	channelIDs := expandInt64List(ctx, plan.ChannelIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	body := map[string]any{
		"name":         plan.Name.ValueString(),
		"contract_ids": contractIDs,
		"enabled":      plan.Enabled.ValueBool(),
		"channel_ids":  channelIDs,
	}

	var result apiMonitor
	if err := r.client.create(ctx, "/monitors", body, &result); err != nil {
		resp.Diagnostics.AddError("Create monitor failed", err.Error())
		return
	}

	plan.ID = types.Int64Value(result.ID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MonitorResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state MonitorResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result apiMonitor
	path := fmt.Sprintf("/monitors/%d", state.ID.ValueInt64())
	if err := r.client.get(ctx, path, &result); err != nil {
		resp.Diagnostics.AddError("Read monitor failed", err.Error())
		return
	}

	state.Name = types.StringValue(result.Name)
	state.Enabled = types.BoolValue(result.Enabled)
	state.ContractIDs, _ = types.ListValueFrom(ctx, types.StringType, result.ContractIDs)
	state.ChannelIDs, _ = types.ListValueFrom(ctx, types.Int64Type, result.ChannelIDs)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *MonitorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan MonitorResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	contractIDs := expandStringList(ctx, plan.ContractIDs, &resp.Diagnostics)
	channelIDs := expandInt64List(ctx, plan.ChannelIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	body := map[string]any{
		"name":         plan.Name.ValueString(),
		"contract_ids": contractIDs,
		"enabled":      plan.Enabled.ValueBool(),
		"channel_ids":  channelIDs,
	}

	var result apiMonitor
	path := fmt.Sprintf("/monitors/%d", plan.ID.ValueInt64())
	if err := r.client.update(ctx, path, body, &result); err != nil {
		resp.Diagnostics.AddError("Update monitor failed", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MonitorResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state MonitorResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	path := fmt.Sprintf("/monitors/%d", state.ID.ValueInt64())
	if err := r.client.delete(ctx, path); err != nil {
		resp.Diagnostics.AddError("Delete monitor failed", err.Error())
		return
	}
}

func (r *MonitorResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := strconv.ParseInt(req.ID, 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", "Expected a numeric monitor ID")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), types.Int64Value(id))...)
}

// --- API response types ---

type apiMonitor struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	ContractIDs []string `json:"contract_ids"`
	Enabled     bool     `json:"enabled"`
	ChannelIDs  []int64  `json:"channel_ids"`
}
