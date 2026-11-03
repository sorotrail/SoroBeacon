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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	defer func() {
		_ = resp.Body.Close()
	}()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// Stable machine-readable error codes matching the server API contract.
const (
	CodeNotFound        = "NOT_FOUND"
	CodeAlreadyExists   = "ALREADY_EXISTS"
	CodeInvalidArgument = "INVALID_ARGUMENT"
	CodeConflict        = "CONFLICT"
	CodeRateLimited     = "RATE_LIMITED"
	CodeInternalError   = "INTERNAL_ERROR"
)

// APIError represents a structured error returned from the SoroBeacon API.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Details   []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("API error %d (%s)", e.Status, e.Code)
}

// IsNotFound reports whether err indicates the target resource does not exist.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == CodeNotFound || apiErr.Status == http.StatusNotFound
	}
	return err != nil && (strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "404"))
}

// IsAlreadyExists reports whether err indicates a resource conflict or unique constraint failure.
func IsAlreadyExists(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == CodeAlreadyExists || apiErr.Code == "DUPLICATE" || apiErr.Status == http.StatusConflict
	}
	return false
}

// IsInvalidArgument reports whether err represents invalid input or a validation rejection.
func IsInvalidArgument(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Code == CodeAlreadyExists || apiErr.Code == "DUPLICATE" {
			return false
		}
		return apiErr.Code == CodeInvalidArgument || apiErr.Status == http.StatusBadRequest
	}
	return false
}

func decodeAPIError(status int, data []byte) error {
	apiErr := &APIError{
		Status:  status,
		Code:    http.StatusText(status),
		Message: http.StatusText(status),
	}
	var env struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
		Details   []struct {
			Field  string `json:"field"`
			Reason string `json:"reason"`
		} `json:"details"`
	}
	if err := json.Unmarshal(data, &env); err == nil {
		if env.Error != "" {
			apiErr.Message = env.Error
		}
		if env.Code != "" {
			apiErr.Code = env.Code
		}
		apiErr.RequestID = env.RequestID
		apiErr.Details = env.Details
	} else if len(data) > 0 {
		apiErr.Message = string(data)
	}
	return apiErr
}

func (c *apiClient) get(ctx context.Context, path string, result any) error {
	data, status, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return decodeAPIError(status, data)
	}
	return json.Unmarshal(data, result)
}

func (c *apiClient) create(ctx context.Context, path string, body any, result any) error {
	data, status, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return decodeAPIError(status, data)
	}
	return json.Unmarshal(data, result)
}

func (c *apiClient) update(ctx context.Context, path string, body any, result any) error {
	data, status, err := c.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return decodeAPIError(status, data)
	}
	return json.Unmarshal(data, result)
}

func (c *apiClient) delete(ctx context.Context, path string) error {
	data, status, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return decodeAPIError(status, data)
	}
	return nil
}
