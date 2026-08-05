{{/* Chart name (optionally overridden). */}}
{{- define "o3k-horizon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Release-prefixed fullname. */}}
{{- define "o3k-horizon.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "o3k-horizon.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "o3k-horizon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "o3k-horizon.labels" -}}
helm.sh/chart: {{ include "o3k-horizon.chart" . }}
{{ include "o3k-horizon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "o3k-horizon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "o3k-horizon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
