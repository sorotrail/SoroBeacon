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

var _ resource.Resource = (*ChannelResource)(nil)
var _ resource.ResourceWithImportState = (*ChannelResource)(nil)

type ChannelResource struct {
	client *apiClient
}

type ChannelResourceModel struct {
	ID      types.Int64  `tfsdk:"id"`
	Name    types.String `tfsdk:"name"`
	Type    types.String `tfsdk:"type"`
	Config  types.String `tfsdk:"config"`
	Enabled types.Bool   `tfsdk:"enabled"`
}

func NewChannelResource() resource.Resource {
	return &ChannelResource{}
}

func (r *ChannelResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_channel"
}

func (r *ChannelResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a SoroBeacon notification channel.\n\n" +
			"**WARNING: channel config contains secrets** (webhook URLs, bot tokens, SMTP " +
			"credentials). Terraform stores attribute values in plain text in state. " +
			"Always encrypt your state backend (e.g. S3+KMS). The config attribute is " +
			"marked Sensitive so `terraform plan` redacts it, but the state file itself " +
			"is NOT encrypted by Terraform.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				Computed: true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Channel display name.",
			},
			"type": schema.StringAttribute{
				Required:    true,
				Description: "Channel type (slack, discord, telegram, email, webhook, matrix, pagerduty, federation).",
			},
			"config": schema.StringAttribute{
				Required:    true,
				Sensitive:   true,
				Description: "Channel-specific config as a JSON string. Contains secrets — see the warning above.",
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
		},
	}
}

func (r *ChannelResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*apiClient)
}

func (r *ChannelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan ChannelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := map[string]any{
		"name":    plan.Name.ValueString(),
		"type":    plan.Type.ValueString(),
		"config":  jsonRaw(plan.Config.ValueString()),
		"enabled": plan.Enabled.ValueBool(),
	}

	var result apiChannel
	if err := r.client.create(ctx, "/channels", body, &result); err != nil {
		resp.Diagnostics.AddError("Create channel failed", err.Error())
		return
	}

	plan.ID = types.Int64Value(result.ID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ChannelResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state ChannelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result apiChannel
	path := fmt.Sprintf("/channels/%d", state.ID.ValueInt64())
	if err := r.client.get(ctx, path, &result); err != nil {
		resp.Diagnostics.AddError("Read channel failed", err.Error())
		return
	}

	state.Name = types.StringValue(result.Name)
	state.Type = types.StringValue(result.Type)
	state.Enabled = types.BoolValue(result.Enabled)
	// Config is not returned by the API (json:"-"), so we keep the plan value.

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *ChannelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan ChannelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := map[string]any{
		"name":    plan.Name.ValueString(),
		"type":    plan.Type.ValueString(),
		"config":  jsonRaw(plan.Config.ValueString()),
		"enabled": plan.Enabled.ValueBool(),
	}

	var result apiChannel
	path := fmt.Sprintf("/channels/%d", plan.ID.ValueInt64())
	if err := r.client.update(ctx, path, body, &result); err != nil {
		resp.Diagnostics.AddError("Update channel failed", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ChannelResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state ChannelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	path := fmt.Sprintf("/channels/%d", state.ID.ValueInt64())
	if err := r.client.delete(ctx, path); err != nil {
		resp.Diagnostics.AddError("Delete channel failed", err.Error())
		return
	}
}

func (r *ChannelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := strconv.ParseInt(req.ID, 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", "Expected a numeric channel ID")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), types.Int64Value(id))...)
}

type apiChannel struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}
