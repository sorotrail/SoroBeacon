package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"
)

// jsonTemplateConfig holds the configuration for the JSON template channel.
type jsonTemplateConfig struct {
	URL          string            `json:"url"`
	Method       string            `json:"method"`
	Headers      map[string]string `json:"headers"`
	BodyTemplate string            `json:"body_template"`
}

// JSONTemplate renders a user-supplied Go text/template against the alert and
// sends the result to a configured URL with configured headers.
type JSONTemplate struct {
	cfg         jsonTemplateConfig
	bodyTpl     *template.Template
	allowedMethods map[string]struct{}
}

// NewJSONTemplate builds a JSON template notifier from channel config.
func NewJSONTemplate(config json.RawMessage) (Notifier, error) {
	var cfg jsonTemplateConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("jsontemplate: invalid config: %w", err)
	}

	if cfg.URL == "" {
		return nil, fmt.Errorf("jsontemplate: url is required")
	}
	if strings.TrimSpace(cfg.BodyTemplate) == "" {
		return nil, fmt.Errorf("jsontemplate: body_template is required")
	}

	// Parse the template once at construction time so a malformed template
	// fails when the channel is created, not on the first alert.
	bodyTpl, err := template.New("body").Parse(cfg.BodyTemplate)
	if err != nil {
		return nil, fmt.Errorf("jsontemplate: invalid body_template: %w", err)
	}

	method := strings.ToUpper(strings.TrimSpace(cfg.Method))
	if method == "" {
		method = http.MethodPost
	}

	allowedMethods := map[string]struct{}{
		http.MethodPost:  {},
		http.MethodPut:   {},
		http.MethodPatch: {},
	}
	if _, ok := allowedMethods[method]; !ok {
		return nil, fmt.Errorf("jsontemplate: method must be one of POST, PUT, PATCH")
	}
	cfg.Method = method

	return &JSONTemplate{
		cfg:            cfg,
		bodyTpl:        bodyTpl,
		allowedMethods: allowedMethods,
	}, nil
}

// Send renders the body template with the alert data and sends it to the
// configured endpoint.
func (j *JSONTemplate) Send(ctx context.Context, a Alert) error {
	var body strings.Builder
	if err := j.bodyTpl.Execute(&body, a); err != nil {
		return fmt.Errorf("jsontemplate: render body: %w", err)
	}

	// Use the shared HTTP client with the configured method and headers.
	// Header values are secrets: they must never appear in errors, logs, or
	// delivery response_snippets. The http.requestJSON function already
	// redacts the URL from errors; we must also avoid logging headers.
	if err := requestJSON(ctx, j.cfg.Method, j.cfg.URL, []byte(body.String()), j.cfg.Headers); err != nil {
		return fmt.Errorf("jsontemplate: %w", err)
	}
	return nil
}