// Package provider implements the SoroBeacon Terraform provider using the
// Terraform Plugin Framework (not the deprecated SDKv2).
//
// Design decisions (stated here, defended in the PR):
//
//   - Location: a subdirectory with its own go.mod keeps provider dependencies
//     (terraform-plugin-framework and its transitive tree) out of the server
//     build. Contributors working on the server never download terraform deps
//     and vice versa.
//
//   - Channel secrets: channel config holds credentials. Terraform state stores
//     attribute values IN PLAIN TEXT on disk. This provider marks
//     channel config as Sensitive in the schema so `terraform plan` redacts it,
//     but the state file still contains it. Operators MUST encrypt state at
//     rest (S3 backend + KMS, or similar). This is documented prominently in
//     the README — an operator must not discover it from state.
//
//   - Import: `terraform import` is supported on all three resources so
//     adopting an existing instance is a first-class operation.
//
//   - Scope: monitors, rules and channels. Covering these three well beats
//     covering everything badly. Templates, saved searches and alerts are
//     left out for now — alerts are read-only by nature and the others are
//     low-value for IaC.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = (*SoroBeaconProvider)(nil)

// SoroBeaconProvider implements the Terraform provider for SoroBeacon.
type SoroBeaconProvider struct {
	version string
}

type SoroBeaconProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

// New returns a provider.Provider constructor for providerserver.Serve.
func New() provider.Provider {
	return &SoroBeaconProvider{version: "0.1.0"}
}

func (p *SoroBeaconProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "sorobeacon"
	resp.Version = p.version
}

func (p *SoroBeaconProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage SoroBeacon monitors, rules and channels as code.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Description: "SoroBeacon API base URL (e.g. http://localhost:8080/api/v1).",
				Required:    true,
			},
			"token": schema.StringAttribute{
				Description: "API bearer token. Required when API_TOKEN is set on the instance.",
				Optional:    true,
				Sensitive:   true,
			},
		},
	}
}

func (p *SoroBeaconProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config SoroBeaconProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := &apiClient{
		endpoint:   config.Endpoint.ValueString(),
		token:      config.Token.ValueString(),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *SoroBeaconProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewMonitorResource,
		NewRuleResource,
		NewChannelResource,
	}
}

func (p *SoroBeaconProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewMonitorDataSource,
	}
}

// --- API client ---

type apiClient struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

func (c *apiClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	url := c.endpoint + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

func (c *apiClient) get(ctx context.Context, path string, result any) error {
	data, status, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("not found")
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("API error %d: %s", status, string(data))
	}
	return json.Unmarshal(data, result)
}

func (c *apiClient) create(ctx context.Context, path string, body any, result any) error {
	data, status, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("API error %d: %s", status, string(data))
	}
	return json.Unmarshal(data, result)
}

func (c *apiClient) update(ctx context.Context, path string, body any, result any) error {
	data, status, err := c.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("API error %d: %s", status, string(data))
	}
	return json.Unmarshal(data, result)
}

func (c *apiClient) delete(ctx context.Context, path string) error {
	_, status, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return fmt.Errorf("API error %d on delete", status)
	}
	return nil
}
