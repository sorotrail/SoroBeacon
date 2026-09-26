package notify

import "text/template"

// PerMonitorTemplate supports per-monitor message templates.
type PerMonitorTemplate struct {
	tmpl *template.Template
}

func NewPerMonitorTemplate(pattern string) *PerMonitorTemplate {
	t := template.Must(template.New("monitor").Parse(pattern))
	return &PerMonitorTemplate{tmpl: t}
}
