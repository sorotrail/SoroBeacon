package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*MonitorDataSource)(nil)

type MonitorDataSource struct {
	client *apiClient
}

type MonitorDataSourceModel struct {
	ID          types.Int64  `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	ContractIDs types.List   `tfsdk:"contract_ids"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	ChannelIDs  types.List   `tfsdk:"channel_ids"`
}

func NewMonitorDataSource() datasource.DataSource {
	return &MonitorDataSource{}
}

func (d *MonitorDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_monitor"
}

func (d *MonitorDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Look up an existing SoroBeacon monitor by ID.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				Required: true,
			},
			"name": schema.StringAttribute{
				Computed: true,
			},
			"contract_ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
			},
			"enabled": schema.BoolAttribute{
				Computed: true,
			},
			"channel_ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.Int64Type,
			},
		},
	}
}

func (d *MonitorDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	d.client = req.ProviderData.(*apiClient)
}

func (d *MonitorDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config MonitorDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result apiMonitor
	path := fmt.Sprintf("/monitors/%d", config.ID.ValueInt64())
	if err := d.client.get(ctx, path, &result); err != nil {
		resp.Diagnostics.AddError("Read monitor failed", err.Error())
		return
	}

	config.Name = types.StringValue(result.Name)
	config.Enabled = types.BoolValue(result.Enabled)
	config.ContractIDs, _ = types.ListValueFrom(ctx, types.StringType, result.ContractIDs)
	config.ChannelIDs, _ = types.ListValueFrom(ctx, types.Int64Type, result.ChannelIDs)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
