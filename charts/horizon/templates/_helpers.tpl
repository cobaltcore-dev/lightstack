{{/* Chart name (optionally overridden). */}}
{{- define "lightstack-horizon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Release-prefixed fullname. */}}
{{- define "lightstack-horizon.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "lightstack-horizon.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "lightstack-horizon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "lightstack-horizon.labels" -}}
helm.sh/chart: {{ include "lightstack-horizon.chart" . }}
{{ include "lightstack-horizon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "lightstack-horizon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lightstack-horizon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
