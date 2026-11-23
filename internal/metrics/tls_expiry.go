package metrics

import "github.com/prometheus/client_golang/prometheus"

// RegisterTLSCertificateExpiry registers a GaugeFunc exposing the Unix
// expiration timestamp (in seconds) of the active leaf certificate for the
// named component (e.g. "server", "sorotrail_client", "database_client").
// Because it evaluates notAfterUnix on every Prometheus scrape, live
// certificate reloads via SIGHUP are reflected immediately.
func (m *Metrics) RegisterTLSCertificateExpiry(component string, notAfterUnix func() float64) {
	if m == nil || m.registry == nil || component == "" || notAfterUnix == nil {
		return
	}
	gf := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "sorobeacon_tls_certificate_not_after_timestamp_seconds",
		Help:        "Unix timestamp in seconds when the active TLS leaf certificate expires.",
		ConstLabels: prometheus.Labels{"component": component},
	}, notAfterUnix)
	_ = m.registry.Register(gf)
}
