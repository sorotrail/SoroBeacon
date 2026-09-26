package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// JSONTemplate is a generic notification channel that sends an HTTP request
// with a custom JSON body rendered via a Go text/template.
type JSONTemplate struct {
	url          string
	method       string
	headers      map[string]string
	bodyTemplate *template.Template
}

// JSONTemplateConfig defines the configuration shape for the jsontemplate channel.
type JSONTemplateConfig struct {
	URL          string            `json:"url"`
	Method       string            `json:"method"`
	Headers      map[string]string `json:"headers"`
	BodyTemplate string            `json:"body_template"`
}

// NewJSONTemplate creates a new JSONTemplate notifier from raw JSON config.
func NewJSONTemplate(config json.RawMessage) (Notifier, error) {
	var cfg JSONTemplateConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("jsontemplate: parse config: %w", err)
	}

	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("jsontemplate: url is required")
	}

	method := strings.ToUpper(strings.TrimSpace(cfg.Method))
	if method == "" {
		method = "POST"
	}
	if method != "POST" && method != "PUT" && method != "PATCH" {
		return nil, fmt.Errorf("jsontemplate: method must be one of POST, PUT, PATCH, got %q", cfg.Method)
	}

	if strings.TrimSpace(cfg.BodyTemplate) == "" {
		return nil, errors.New("jsontemplate: body_template is required")
	}

	tmpl, err := template.New("body_template").Parse(cfg.BodyTemplate)
	if err != nil {
		return nil, fmt.Errorf("jsontemplate: invalid body_template: %w", err)
	}

	return &JSONTemplate{
		url:          cfg.URL,
		method:       method,
		headers:      cfg.Headers,
		bodyTemplate: tmpl,
	}, nil
}

// Send renders the template against the alert and executes the HTTP request.
func (j *JSONTemplate) Send(ctx context.Context, a Alert) error {
	var buf bytes.Buffer
	if err := j.bodyTemplate.Execute(&buf, a); err != nil {
		return fmt.Errorf("jsontemplate: render body: %w", err)
	}

	headers := make(map[string]string)
	for k, v := range j.headers {
		headers[k] = v
	}
	if headers["Content-Type"] == "" {
		headers["Content-Type"] = "application/json"
	}

	return requestJSON(ctx, j.method, j.url, buf.Bytes(), headers)
}
