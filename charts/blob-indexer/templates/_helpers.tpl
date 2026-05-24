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
Database URL used by migration containers.
*/}}
{{- define "blob-indexer.migrationDatabaseURL" -}}
{{- $sslMode := .Values.database.sslMode | default "require" -}}
{{- $dbURL := .Values.appConfig.database.url -}}
{{- if .Values.externalDatabase.url -}}
{{- $dbURL = .Values.externalDatabase.url -}}
{{- else if .Values.postgresql.enabled -}}
{{- $dbURL = printf "postgres://%s:%s@%s-postgresql:5432/%s?sslmode=%s" .Values.postgresql.auth.username .Values.postgresql.auth.password .Release.Name .Values.postgresql.auth.database $sslMode -}}
{{- end -}}
{{- $dbURL -}}
{{- end }}

{{/*
Migration init container used when the chart owns the PostgreSQL dependency.
*/}}
{{- define "blob-indexer.migrationInitContainer" -}}
- name: migrate
  image: "{{ .Values.api.image.repository | default .Values.image.repository }}:{{ .Values.api.image.tag | default .Values.image.tag | default .Chart.AppVersion }}"
  imagePullPolicy: {{ .Values.image.pullPolicy }}
  command:
    - ./blob-indexer-migrate
  {{- with .Values.containerSecurityContext }}
  securityContext:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  env:
    - name: DB_URL
      value: {{ include "blob-indexer.migrationDatabaseURL" . | quote }}
    - name: LOG_LEVEL
      value: {{ .Values.appConfig.logging.level | quote }}
    - name: LOG_FORMAT
      value: {{ .Values.appConfig.logging.format | quote }}
  resources:
    {{- toYaml (.Values.api.resources | default .Values.resources) | nindent 4 }}
{{- end }}
