{{/* Chart name. */}}
{{- define "o3k.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Release-prefixed fullname (FR-016: multi-release isolation). */}}
{{- define "o3k.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "o3k.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "o3k.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "o3k.labels" -}}
helm.sh/chart: {{ include "o3k.chart" . }}
{{ include "o3k.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "o3k.selectorLabels" -}}
app.kubernetes.io/name: {{ include "o3k.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Image reference; repository is REQUIRED (FR-018). */}}
{{- define "o3k.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" (required "image.repository is required — the chart pulls no implicit image (FR-018)." .Values.image.repository) $tag -}}
{{- end -}}

{{/* Name of the Secret holding the rendered o3k.yaml. */}}
{{- define "o3k.configSecretName" -}}
{{- .Values.secrets.existingSecret | default (printf "%s-config" (include "o3k.fullname" .)) -}}
{{- end -}}

{{/*
Install-time validation gates (fail-fast). Invoked once from service.yaml (always rendered).
FR-006/006a/007/007a/008b/012e/018.
*/}}
{{- define "o3k.validate" -}}
{{- if not .Values.image.repository -}}
{{- fail "\n\nimage.repository is required (FR-018): the chart pulls no implicit image.\nSet --set image.repository=<registry>/o3k\n" -}}
{{- end -}}
{{- if not .Values.secrets.existingSecret -}}
{{-   if not .Values.secrets.jwtSecret -}}
{{- fail "\n\nsecrets.jwtSecret is required (FR-007): no insecure default. Set it, or supply secrets.existingSecret.\n" -}}
{{-   end -}}
{{-   if not .Values.secrets.adminPassword -}}
{{- fail "\n\nsecrets.adminPassword is required (FR-007a): no deployment may run with admin/secret. Set it, or supply secrets.existingSecret.\n" -}}
{{-   end -}}
{{-   if and (not .Values.database.url) (not .Values.database.existingSecret) -}}
{{- fail "\n\ndatabase.url is required (FR-006): the chart deploys no database — provide a reachable external one.\n" -}}
{{-   end -}}
{{- end -}}
{{- if .Values.tls.enabled -}}
{{-   if and (not .Values.tls.secretName) (not .Values.tls.certManagerIssuer) -}}
{{- fail "\n\ntls.enabled requires tls.secretName or tls.certManagerIssuer (the chart generates no certificates).\n" -}}
{{-   end -}}
{{- end -}}
{{- if and (eq .Values.deploymentMode "multi-node") .Values.realMode.enabled -}}
{{-   if not (or (eq .Values.glance.storageMode "rbd") (eq .Values.glance.storageMode "s3")) -}}
{{- fail "\n\nmulti-node real mode requires glance.storageMode=rbd or s3 (FR-012e): local image storage is node-local.\n" -}}
{{-   end -}}
{{-   if not (eq .Values.cinder.storageMode "rbd") -}}
{{- fail "\n\nmulti-node real mode requires cinder.storageMode=rbd (FR-012e): local volume storage is node-local.\n" -}}
{{-   end -}}
{{- end -}}
{{- if .Values.cache.enabled -}}
{{-   if and (not .Values.cache.redisUrl) (not .Values.secrets.existingSecret) -}}
{{- fail "\n\ncache.enabled requires cache.redisUrl (FR-006a): the chart deploys no Redis.\n" -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
Render the product o3k.yaml from values. Embedded into a Secret (research R2:
the binary has no generic O3K_<SECTION>_<KEY> env layer, so config — including the
admin password — is carried in the --config file, kept out of plaintext via a Secret).
*/}}
{{- define "o3k.config" -}}
database:
  url: {{ .Values.database.url | default "postgres://o3k@REPLACE_ME/o3k" | quote }}
  max_connections: 50
keystone:
  port: 35357
  jwt_secret: {{ .Values.secrets.jwtSecret | quote }}
  token_ttl: 24h
  admin_user: {{ .Values.secrets.adminUser | default "admin" | quote }}
  admin_password: {{ .Values.secrets.adminPassword | quote }}
compute:
  node_id: auto
  tunnel_ip: auto
  vxlan_port: {{ .Values.multiNode.vxlanPort }}
  heartbeat_interval: 30s
nova:
  port: 8774
  libvirt_uri: "qemu:///system"
  libvirt_mode: {{ .Values.nova.libvirtMode | quote }}
neutron:
  port: 9696
  networking_mode: {{ .Values.neutron.networkingMode | quote }}
  security_group_mode: {{ .Values.neutron.securityGroupMode | quote }}
  vxlan_enabled: {{ if eq .Values.deploymentMode "multi-node" }}{{ .Values.multiNode.vxlanEnabled }}{{ else }}false{{ end }}
cinder:
  port: 8776
  storage_mode: {{ .Values.cinder.storageMode | quote }}
glance:
  port: 9292
  storage_mode: {{ .Values.glance.storageMode | quote }}
logging:
  level: {{ .Values.logging.level | quote }}
  format: {{ .Values.logging.format | quote }}
cache:
  enabled: {{ .Values.cache.enabled }}
  redis_url: {{ .Values.cache.redisUrl | quote }}
{{- end -}}
