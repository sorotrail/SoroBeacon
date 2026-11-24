package notify

import "text/template"

// PerMonitorTemplate supports per-monitor message templates.
type PerMonitorTemplate struct {
	tmpl *template.Template
}

// NewPerMonitorTemplate parses pattern into a template that renders a single
// monitor's alert, panicking if pattern is malformed.
func NewPerMonitorTemplate(pattern string) *PerMonitorTemplate {
	t := template.Must(template.New("monitor").Parse(pattern))
	return &PerMonitorTemplate{tmpl: t}
}
