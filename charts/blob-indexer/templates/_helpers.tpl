{{/*
Expand the name of the chart.
*/}}
{{- define "blob-indexer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "blob-indexer.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "blob-indexer.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "blob-indexer.labels" -}}
helm.sh/chart: {{ include "blob-indexer.chart" . }}
{{ include "blob-indexer.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "blob-indexer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "blob-indexer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name
*/}}
{{- define "blob-indexer.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (printf "%s-sa" (include "blob-indexer.fullname" .)) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Database URL used by migration containers when the DB Secret is chart-created.
*/}}
{{- define "blob-indexer.migrationDatabaseURL" -}}
{{- $dbURL := .Values.appConfig.database.url -}}
{{- if .Values.externalDatabase.url -}}
{{- $dbURL = .Values.externalDatabase.url -}}
{{- end -}}
{{- $dbURL -}}
{{- end }}

{{/*
Database Secret name used by the API and indexer.
*/}}
{{- define "blob-indexer.applicationDatabaseSecretName" -}}
{{- if .Values.databaseSecret.name -}}
{{- .Values.databaseSecret.name -}}
{{- else -}}
{{ include "blob-indexer.fullname" . }}-db
{{- end -}}
{{- end }}

{{/*
Database Secret key used by the API and indexer.
*/}}
{{- define "blob-indexer.applicationDatabaseSecretKey" -}}
{{- default "DB_URL" .Values.databaseSecret.key -}}
{{- end }}

{{/*
Database Secret name used by migration containers.
*/}}
{{- define "blob-indexer.databaseSecretName" -}}
{{- if .Values.migrations.databaseSecret.name -}}
{{- .Values.migrations.databaseSecret.name -}}
{{- else -}}
{{ include "blob-indexer.applicationDatabaseSecretName" . }}
{{- end -}}
{{- end }}

{{/*
Database Secret key used by migration containers.
*/}}
{{- define "blob-indexer.databaseSecretKey" -}}
{{- default (include "blob-indexer.applicationDatabaseSecretKey" .) .Values.migrations.databaseSecret.key -}}
{{- end }}

{{/*
Whether migration containers should read DB_URL from a Secret.
*/}}
{{- define "blob-indexer.useMigrationDatabaseSecret" -}}
{{- if or .Values.migrations.databaseSecret.name (not .Values.databaseSecret.create) -}}
true
{{- end -}}
{{- end }}

{{/*
Name of the Secret that holds per-network RPC URLs.

When rpcSecret.existingSecret is set, that pre-provisioned Secret is used and the
chart renders no Secret of its own. Otherwise the chart manages a Secret named
"<fullname>-rpc" (overridable via rpcSecret.name).
*/}}
{{- define "blob-indexer.rpcSecretName" -}}
{{- if .Values.rpcSecret.existingSecret -}}
{{- .Values.rpcSecret.existingSecret -}}
{{- else if .Values.rpcSecret.name -}}
{{- .Values.rpcSecret.name -}}
{{- else -}}
{{ include "blob-indexer.fullname" . }}-rpc
{{- end -}}
{{- end }}

{{/*
Whether the chart should render its own RPC Secret. Only true when an
existingSecret is not provided and at least one network ships an rpc_url through
values (chart-managed Secret path). This does not filter on the network's
`enabled` flag — a values-supplied rpc_url is sufficient to manage a Secret key
for it.
*/}}
{{- define "blob-indexer.createRpcSecret" -}}
{{- if not .Values.rpcSecret.existingSecret -}}
{{- range .Values.appConfig.networks -}}
{{- if .rpc_url -}}
true
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Environment variable name carrying a network's RPC URL.

The application reads overrides as exactly "NETWORK_" + strings.ToUpper(name) +
"_RPC_URL" (internal/config/config.go) with NO character substitution, so this
helper must match byte-for-byte: it only uppercases the name. Network names must
therefore be limited to characters valid in a Kubernetes env var name / Secret
key ([A-Za-z0-9_.-]); the built-in networks (mainnet, sepolia, holesky) already
satisfy this. Normalising here would silently break the override lookup for any
name containing "-" or "." (e.g. chart "NETWORK_OP_MAINNET_RPC_URL" vs app
"NETWORK_OP-MAINNET_RPC_URL").
*/}}
{{- define "blob-indexer.rpcEnvName" -}}
{{- $name := required "appConfig.networks[].name is required" . | toString | upper -}}
{{- printf "NETWORK_%s_RPC_URL" $name -}}
{{- end }}

{{/*
Render the NETWORK_<NAME>_RPC_URL env entries (sourced from the RPC Secret) for
every enabled network that has an rpc_url, OR every network when an
existingSecret is provided (the operator owns which keys exist there). Emits
nothing when neither an existingSecret nor any values-supplied rpc_url is set,
which is the credential-free default for local/dev installs.
*/}}
{{- define "blob-indexer.rpcEnv" -}}
{{- $root := . -}}
{{- $secretName := include "blob-indexer.rpcSecretName" $root -}}
{{- range $root.Values.appConfig.networks -}}
{{- if or $root.Values.rpcSecret.existingSecret .rpc_url -}}
{{- $envName := include "blob-indexer.rpcEnvName" .name }}
- name: {{ $envName }}
  valueFrom:
    secretKeyRef:
      name: {{ $secretName }}
      key: {{ $envName }}
      {{- /*
        optional so that, with an existingSecret, a network whose key the
        operator has not (yet) provisioned does not block pod startup. The app
        falls back to the rpc_url from config (empty by default) for that
        network; the indexer fails fast on an empty RPC URL for an enabled
        network, surfacing the misconfiguration clearly.
      */}}
      optional: true
{{ end -}}
{{- end -}}
{{- end }}

{{/*
Build a container image reference from the chart defaults and a component override.
*/}}
{{- define "blob-indexer.image" -}}
{{- $root := index . 0 -}}
{{- $image := index . 1 -}}
{{- $repository := default $root.Values.image.repository $image.repository -}}
{{- $tag := default (default $root.Chart.AppVersion $root.Values.image.tag) $image.tag -}}
{{- if $root.Values.image.registry -}}
{{- printf "%s/%s:%s" (trimSuffix "/" $root.Values.image.registry) $repository $tag -}}
{{- else -}}
{{- printf "%s:%s" $repository $tag -}}
{{- end -}}
{{- end }}
