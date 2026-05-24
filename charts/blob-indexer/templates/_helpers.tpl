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
