{{/* Common naming, mirroring the sibling SoroTrail chart. */}}
{{- define "sorobeacon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "sorobeacon.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "sorobeacon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "sorobeacon.labels" -}}
helm.sh/chart: {{ include "sorobeacon.chart" . }}
{{ include "sorobeacon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "sorobeacon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sorobeacon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "sorobeacon.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "sorobeacon.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Name of the Secret holding DATABASE_URL: the operator's, or ours. */}}
{{- define "sorobeacon.databaseSecretName" -}}
{{- if .Values.database.existingSecret }}
{{- .Values.database.existingSecret }}
{{- else }}
{{- include "sorobeacon.fullname" . }}-db
{{- end }}
{{- end }}

{{/*
Key holding DATABASE_URL. A chart-managed Secret always uses the variable
name, so existingSecretKey only applies to an operator-provided Secret;
otherwise the deployment would look up a key our own Secret does not define.
*/}}
{{- define "sorobeacon.databaseSecretKey" -}}
{{- if .Values.database.existingSecret }}{{ .Values.database.existingSecretKey }}{{ else }}DATABASE_URL{{ end -}}
{{- end -}}

{{/* Name of the Secret holding API_TOKEN and CONFIG_ENCRYPTION_KEY. */}}
{{- define "sorobeacon.authSecretName" -}}
{{- if .Values.auth.existingSecret }}
{{- .Values.auth.existingSecret }}
{{- else }}
{{- include "sorobeacon.fullname" . }}-auth
{{- end }}
{{- end }}

{{- define "sorobeacon.authAPITokenKey" -}}
{{- if .Values.auth.existingSecret }}{{ .Values.auth.apiTokenKey }}{{ else }}API_TOKEN{{ end -}}
{{- end -}}

{{- define "sorobeacon.authConfigEncryptionKeyKey" -}}
{{- if .Values.auth.existingSecret }}{{ .Values.auth.configEncryptionKeyKey }}{{ else }}CONFIG_ENCRYPTION_KEY{{ end -}}
{{- end -}}

{{/*
Labels for the migration hook pod. Deliberately NOT sorobeacon.selectorLabels:
the Service selects pods by that pair, so a hook pod carrying it would be
routed dashboard and API traffic for as long as the hook runs.
*/}}
{{- define "sorobeacon.migrateSelectorLabels" -}}
app.kubernetes.io/name: {{ include "sorobeacon.name" . }}-migrate
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
sorobeacon.configData is the application's non-secret configuration as
ConfigMap data. It is defined once and included by both the release ConfigMap
and the migration hook's own ConfigMap, so the two cannot drift: adding a new
environment variable to internal/config means adding one line here and nothing
else. Values are quoted so that numbers, durations and booleans all land as
strings, which is what the process environment requires.

Whole numbers go through int64: Helm parses YAML integers as floats, so a plain
`quote` would render 1048576 as "1.048576e+06" and the app would reject it.
*/}}
{{- define "sorobeacon.configData" -}}
CONFIG_FILE: {{ .Values.config.configFile | quote }}
NETWORK: {{ .Values.config.network | quote }}
RPC_URL: {{ .Values.config.rpcUrl | quote }}
NETWORK_PASSPHRASE: {{ .Values.config.networkPassphrase | quote }}
SOURCE_MODE: {{ .Values.config.sourceMode | quote }}
SOROTRAIL_URL: {{ .Values.config.sorotrailUrl | quote }}
HORIZON_URL: {{ .Values.config.horizonUrl | quote }}
GRAPHQL_PLAYGROUND: {{ .Values.config.graphqlPlayground | toString | quote }}
GRAPHQL_MAX_DEPTH: {{ .Values.config.graphqlMaxDepth | quote }}
GRAPHQL_MAX_COMPLEXITY: {{ .Values.config.graphqlMaxComplexity | quote }}
HTTP_ADDR: {{ .Values.config.httpAddr | quote }}
HTTP_MAX_BODY_BYTES: {{ .Values.config.httpMaxBodyBytes | int64 | quote }}
POLL_INTERVAL: {{ .Values.config.pollInterval | quote }}
LOG_LEVEL: {{ .Values.config.logLevel | quote }}
MONITOR_SILENT_AFTER: {{ .Values.config.monitorSilentAfter | quote }}
READYZ_LAG_THRESHOLD: {{ .Values.config.readyzLagThreshold | int64 | quote }}
CHANNEL_DISABLE_AFTER_FAILURES: {{ .Values.config.channelDisableAfterFailures | int64 | quote }}
NETWORKS: {{ .Values.config.networks | quote }}
OIDC_ISSUER: {{ .Values.oidc.issuer | quote }}
OIDC_CLIENT_ID: {{ .Values.oidc.clientID | quote }}
OIDC_REDIRECT_URL: {{ .Values.oidc.redirectURL | quote }}
OIDC_SCOPES: {{ .Values.oidc.scopes | quote }}
OIDC_ALLOWED_DOMAINS: {{ .Values.oidc.allowedDomains | quote }}
OIDC_WORKSPACE: {{ .Values.oidc.workspace | quote }}
OIDC_WORKSPACE_CLAIM: {{ .Values.oidc.workspaceClaim | quote }}
OIDC_LOGIN_STATE_TTL: {{ .Values.oidc.loginStateTTL | quote }}
RATE_LIMIT_RPS: {{ .Values.config.rateLimitRps | quote }}
RATE_LIMIT_BURST: {{ .Values.config.rateLimitBurst | int64 | quote }}
RATE_LIMIT_TRUST_FORWARDED: {{ .Values.config.rateLimitTrustForwarded | toString | quote }}
CORS_ALLOWED_ORIGINS: {{ .Values.config.corsAllowedOrigins | quote }}
ALERT_RETENTION: {{ .Values.config.alertRetention | quote }}
DATABASE_MAX_CONNS: {{ .Values.database.maxConns | int64 | quote }}
DATABASE_MIN_CONNS: {{ .Values.database.minConns | int64 | quote }}
DATABASE_MAX_CONN_LIFETIME: {{ .Values.database.maxConnLifetime | quote }}
DATABASE_MAX_CONN_IDLE_TIME: {{ .Values.database.maxConnIdleTime | quote }}
RPC_URLS: {{ .Values.config.rpcUrls | quote }}
GRPC_ADDR: {{ .Values.config.grpcAddr | quote }}
REORG_TRACKING_WINDOW: {{ .Values.config.reorgTrackingWindow | quote }}
REORG_CONFIRMATION_DEPTH: {{ .Values.config.reorgConfirmationDepth | quote }}
ALERT_ENRICHMENT_URL: {{ .Values.config.alertEnrichmentUrl | quote }}
ALERT_ENRICHMENT_TIMEOUT: {{ .Values.config.alertEnrichmentTimeout | quote }}
ALERT_ENRICHMENT_CACHE_TTL: {{ .Values.config.alertEnrichmentCacheTtl | quote }}
ARCHIVE_URL: {{ .Values.config.archiveUrl | quote }}
NOTIFY_RATE_LIMIT_DEFAULT_RPS: {{ .Values.config.notifyRateLimitDefaultRps | quote }}
NOTIFY_RATE_LIMIT_SLACK_RPS: {{ .Values.config.notifyRateLimitSlackRps | quote }}
NOTIFY_RATE_LIMIT_TELEGRAM_RPS: {{ .Values.config.notifyRateLimitTelegramRps | quote }}
NOTIFY_RATE_LIMIT_PAGERDUTY_RPS: {{ .Values.config.notifyRateLimitPagerdutyRps | quote }}
OTLP_ENDPOINT: {{ .Values.config.otlpEndpoint | quote }}
OTLP_SERVICE_NAME: {{ .Values.config.otlpServiceName | quote }}
OTLP_SAMPLE_RATE: {{ .Values.config.otlpSampleRate | quote }}
SECRETS_PROVIDER: {{ .Values.secrets.provider | quote }}
SECRETS_CACHE_TTL: {{ .Values.secrets.cacheTtl | quote }}
VAULT_ADDR: {{ .Values.secrets.vaultAddr | quote }}
VAULT_NAMESPACE: {{ .Values.secrets.vaultNamespace | quote }}
{{- end }}
