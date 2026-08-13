{{/* Chart name, overridable. */}}
{{- define "sluice.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified release name. */}}
{{- define "sluice.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "sluice.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "sluice.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: control-plane
{{- end -}}

{{- define "sluice.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sluice.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "sluice.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "sluice.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Render the control-plane configuration from values.

Built here rather than shipped as a static file so backends and routes stay
ordinary Helm values that an operator can patch, and so a bad edit fails at
template time rather than at container start.
*/}}
{{- define "sluice.config" -}}
{{- $cfg := dict
  "listen" (dict "api" (printf ":%d" (int .Values.service.httpPort)) "authz" (printf ":%d" (int .Values.service.authzPort)))
  "backends" .Values.backends
  "routes" .Values.routes
  "policy" (dict "file" "/etc/sluice/policies.sluice" "watch" true "cacheSize" 8192 "cacheTtlSeconds" 5)
  "pricing" (dict "live" .Values.pricing.live "refreshSeconds" .Values.pricing.refreshSeconds "overrides" .Values.pricing.overrides)
  "carbon" (dict "energyKwhPerGb" .Values.carbon.energyKwhPerGb "refreshSeconds" .Values.carbon.refreshSeconds)
  "router" .Values.router
  "probe" .Values.probe
  "ledger" .Values.ledger
  "demo" (dict "enabled" false)
-}}
{{- toPrettyJson $cfg -}}
{{- end -}}
