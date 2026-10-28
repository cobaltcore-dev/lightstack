{{/* Chart name. */}}
{{- define "lightstack.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Release-prefixed fullname (FR-016: multi-release isolation). */}}
{{- define "lightstack.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "lightstack.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "lightstack.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "lightstack.labels" -}}
helm.sh/chart: {{ include "lightstack.chart" . }}
{{ include "lightstack.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "lightstack.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lightstack.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Selector labels for the console proxy (SPEC-021).

Deliberately a DIFFERENT app.kubernetes.io/name, not lightstack.selectorLabels plus a
component label: the main Service and Deployment select on name+instance alone, so
a console-proxy pod carrying those two would be adopted as a lightstack endpoint and API
traffic would be routed to the noVNC proxy.
*/}}
{{- define "lightstack.consoleProxySelectorLabels" -}}
app.kubernetes.io/name: {{ include "lightstack.name" . }}-console-proxy
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Image reference; repository is REQUIRED (FR-018). */}}
{{- define "lightstack.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" (required "image.repository is required — the chart pulls no implicit image (FR-018)." .Values.image.repository) $tag -}}
{{- end -}}

{{/* Name of the Secret holding the rendered lightstack.yaml. */}}
{{- define "lightstack.configSecretName" -}}
{{- .Values.secrets.existingSecret | default (printf "%s-config" (include "lightstack.fullname" .)) -}}
{{- end -}}

{{/* Teardown (pre-delete hook) resource name prefix (SPEC-010). */}}
{{- define "lightstack.teardown.fullname" -}}
{{- printf "%s-teardown" (include "lightstack.fullname" .) -}}
{{- end -}}

{{/* kubectl image for the teardown quiesce/PVC Job; required under dataRetentionPolicy=delete (FR-018/R1). */}}
{{- define "lightstack.teardown.kubectlImage" -}}
{{- required "teardown.kubectlImage is required when dataRetentionPolicy=delete (no implicit image pull; FR-018 / issue #53)." .Values.teardown.kubectlImage -}}
{{- end -}}

{{/*
Install-time validation gates (fail-fast). Invoked once from service.yaml (always rendered).
FR-006/006a/007/007a/008b/012e/018.
*/}}
{{- define "lightstack.validate" -}}
{{- if not .Values.image.repository -}}
{{- fail "\n\nimage.repository is required (FR-018): the chart pulls no implicit image.\nSet --set image.repository=<registry>/lightstack\n" -}}
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
{{-   if and (include "lightstack.consoleProxyDeployed" .) (not .Values.nova.consoleProxyBaseUrl) -}}
{{- fail "\n\ntls.enabled with the console proxy requires nova.consoleProxyBaseUrl: the proxy serves plain HTTP, so set an https origin that terminates TLS in front of it (or consoleProxy.enabled=false).\n" -}}
{{-   end -}}
{{- end -}}
{{- /* 6080 is the binary's defaultConsoleProxyPort, the only port it derives an origin for. */ -}}
{{- if and (include "lightstack.consoleProxyDeployed" .) (ne (int .Values.consoleProxy.service.port) 6080) (not .Values.nova.consoleProxyBaseUrl) -}}
{{- fail "\n\nconsoleProxy.service.port other than 6080 requires nova.consoleProxyBaseUrl: without one, Lightstack derives console links at port 6080, where this proxy does not listen.\n" -}}
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
{{- if eq .Values.dataRetentionPolicy "delete" -}}
{{-   if not .Values.teardown.kubectlImage -}}
{{- fail "\n\ndataRetentionPolicy=delete requires teardown.kubectlImage (SPEC-010 / issue #53): the chart pulls no implicit image (FR-018).\nSet --set teardown.kubectlImage=<registry>/kubectl:<tag>\n" -}}
{{-   end -}}
{{- end -}}
{{- if ge (int .Values.drainSeconds) (int .Values.terminationGracePeriodSeconds) -}}
{{- fail (printf "\n\ndrainSeconds (%d) must be less than terminationGracePeriodSeconds (%d) (SPEC-013 FR-009): otherwise the SIGTERM drain sleep outlasts the grace period and the kubelet SIGKILLs mid-drain, dropping in-flight requests and leaving endpoint deregistration incomplete.\n" (int .Values.drainSeconds) (int .Values.terminationGracePeriodSeconds)) -}}
{{- end -}}
{{- /* The database backend and URL are configured only through database.url / database.existingSecret,
       which drive validation, the hostPath mounts, startup and the teardown purge together. extraEnv is
       applied last in the container env, so an override there would win for the workload yet never reach
       the mounts or the purge Job (which does not inherit extraEnv) — refuse it (SPEC-029). */ -}}
{{- range $e := .Values.extraEnv -}}
{{-   if or (eq $e.name "LIGHTSTACK_DB_URL") (eq $e.name "LIGHTSTACK_DB_BACKEND") -}}
{{- fail (printf "\n\nextraEnv must not set %s: the database backend and URL are configured only through database.url (or database.existingSecret), which drive validation, the hostPath mounts, startup and the teardown purge together. An override in extraEnv wins for the workload container but not for the mounts or the purge Job (which does not inherit extraEnv), so the database would diverge from where it is mounted and purged. Put the database settings in database.url; keep extraEnv for other variables.\n" $e.name) -}}
{{-   end -}}
{{- end -}}
{{- /* SQLite single-node guards (SPEC-029 FR-002/003a/008/009). Keyed off database.url's
       scheme, so they never fire for PostgreSQL. Ordered: Secret conflict, path canonical,
       path under root, path not in a storage tree, mode, replicas, node pin — the message
       names exactly what is wrong. */ -}}
{{- if eq (include "lightstack.dbBackend" .) "sqlite" -}}
{{-   if or .Values.database.existingSecret .Values.secrets.existingSecret -}}
{{- fail "\n\nSQLite is selected only through database.url and cannot be combined with database.existingSecret or secrets.existingSecret (SPEC-029 FR-003a): a Secret-injected LIGHTSTACK_DB_URL would override it, so the rendered SQLite storage would serve a URL that never takes effect. Use database.url alone for SQLite.\n" -}}
{{-   end -}}
{{-   $path := trimPrefix "sqlite://" .Values.database.url -}}
{{-   $root := .Values.realMode.hostStoragePath -}}
{{-   if or (contains "%" $path) (ne $path (clean $path)) -}}
{{- fail (printf "\n\ndatabase.url SQLite path %q must be a canonical absolute path (SPEC-029 FR-002): no '..' segments, no repeated or trailing slashes, and no percent-encoding. The chart compares this string against realMode.hostStoragePath and mounts its parent directory as a hostPath, while the binary percent-decodes the URL, so a non-canonical or encoded path could pass the prefix check yet open the database outside the mounted directory, where it is lost on pod recreation.\n" $path) -}}
{{-   end -}}
{{-   if not (hasPrefix (printf "%s/" $root) $path) -}}
{{- fail (printf "\n\ndatabase.url SQLite path %q must lie under realMode.hostStoragePath (%s) (SPEC-029 FR-002): the chart mounts that directory as a hostPath, so an outside path is refused.\n" $path $root) -}}
{{-   end -}}
{{-   range $tree := (list "images" "volumes" "instances" "cloud-init" "dhcp" "console-tokens" "console-sockets") -}}
{{-     if hasPrefix (printf "%s/%s/" $root $tree) $path -}}
{{- fail (printf "\n\ndatabase.url SQLite path %q must not lie inside the node-local storage directory %s/%s (SPEC-029 FR-002): teardown removes that tree wholesale before it knows whether the run succeeded, so a database nested there would be deleted with it. Use a separate directory such as %s/db.\n" $path $root $tree $root) -}}
{{-     end -}}
{{-   end -}}
{{-   if ne .Values.deploymentMode "single-node" -}}
{{- fail (printf "\n\nSQLite requires deploymentMode=single-node (got %q) (SPEC-029 FR-009): SQLite serves one node; a multi-node deployment must use PostgreSQL.\n" .Values.deploymentMode) -}}
{{-   end -}}
{{-   if gt (int .Values.replicaCount) 1 -}}
{{- fail (printf "\n\nSQLite requires replicaCount<=1 (got %d) (SPEC-029 FR-009): one pod opens the single database file.\n" (int .Values.replicaCount)) -}}
{{-   end -}}
{{-   $pinned := false -}}
{{-   if .Values.nodeSelector -}}
{{-     if and (hasKey .Values.nodeSelector "kubernetes.io/hostname") (ne (index .Values.nodeSelector "kubernetes.io/hostname" | toString) "") -}}
{{-       $pinned = true -}}
{{-     end -}}
{{-   end -}}
{{-   if and (not $pinned) .Values.affinity .Values.affinity.nodeAffinity .Values.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution -}}
{{-     if ne (include "lightstack.sqliteAffinityIdentity" .) "" -}}
{{-       $pinned = true -}}
{{-     end -}}
{{-   end -}}
{{-   if not $pinned -}}
{{- fail "\n\nSQLite requires a single-node pin (SPEC-029 FR-008): set nodeSelector with a non-empty kubernetes.io/hostname (e.g. nodeSelector: { kubernetes.io/hostname: <node> }), or an affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution whose every nodeSelectorTerm constrains kubernetes.io/hostname (or metadata.name via matchFields) with operator In to the SAME single node. A group selector such as kubernetes.io/os: linux, a required affinity that allows more than one node (multiple hostnames, differing terms, or no hostname constraint), a soft (preferred) affinity, or no pin at all does not bind the pod to one node: a rescheduled pod would find an empty directory and SQLite would start a fresh database there while the data stays on the old node, and the teardown purge Job could run on a different node from the database.\n" -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
lightstack.sqliteNodeSelectorTermIdentity — given one nodeSelectorTerm (the template context),
renders the single node identity it pins to, tagged by kind: "hostname=<v>" from a
kubernetes.io/hostname matchExpression (matchExpressions select by node labels), or "name=<v>" from
a metadata.name matchFields entry (matchFields select by node fields; metadata.name is the only
one Kubernetes supports). Each needs operator In and exactly one non-empty value. Renders "" when
the term does not constrain the node to one identity — a metadata.name in matchExpressions is an
ordinary label many nodes can share, and a kubernetes.io/hostname in matchFields is not a valid
field, so neither counts. The kind tag keeps a hostname label and a metadata.name field with the
same value distinct, since the kubelet hostname label need not equal the node name. Extra
predicates in the term are ignored — they only narrow further (SPEC-029 FR-008).
*/}}
{{- define "lightstack.sqliteNodeSelectorTermIdentity" -}}
{{- $id := "" -}}
{{- range (default (list) .matchExpressions) -}}
{{-   if and (eq .key "kubernetes.io/hostname") (eq (.operator | toString) "In") (eq (len (default (list) .values)) 1) (ne (index .values 0 | toString) "") -}}
{{-     $id = printf "hostname=%s" (index .values 0 | toString) -}}
{{-   end -}}
{{- end -}}
{{- range (default (list) .matchFields) -}}
{{-   if and (eq .key "metadata.name") (eq (.operator | toString) "In") (eq (len (default (list) .values)) 1) (ne (index .values 0 | toString) "") -}}
{{-     $id = printf "name=%s" (index .values 0 | toString) -}}
{{-   end -}}
{{- end -}}
{{- $id -}}
{{- end -}}

{{/*
lightstack.sqliteAffinityIdentity — renders the single node identity enforced across EVERY OR term
of affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution, or "" when the affinity
does not bind the pod to one node (an empty term list, a term with no single non-empty hostname
identity, or terms that resolve to different nodes). SPEC-029 FR-008.
*/}}
{{- define "lightstack.sqliteAffinityIdentity" -}}
{{- $terms := .Values.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms -}}
{{- $identity := "" -}}
{{- $valid := gt (len (default (list) $terms)) 0 -}}
{{- range $t := (default (list) $terms) -}}
{{-   $tid := include "lightstack.sqliteNodeSelectorTermIdentity" $t -}}
{{-   if eq $tid "" -}}
{{-     $valid = false -}}
{{-   else if eq $identity "" -}}
{{-     $identity = $tid -}}
{{-   else if ne $identity $tid -}}
{{-     $valid = false -}}
{{-   end -}}
{{- end -}}
{{- if $valid -}}{{ $identity }}{{- end -}}
{{- end -}}

{{/*
Render the product lightstack.yaml from values. Embedded into a Secret (research R2:
the binary has no generic lightstack_<SECTION>_<KEY> env layer, so config — including the
admin password — is carried in the --config file, kept out of plaintext via a Secret).
*/}}
{{/*
The noVNC console proxy runs only where it can work: real mode, single node (SPEC-021
v1). Its token projection and every guest's console socket are node-local, so in
multi-node one proxy would serve one node and silently fail every other. The binary is
told the same answer (nova.console_enabled) so it refuses consoles instead of issuing
URLs that lead nowhere. Renders "true" or nothing.
*/}}
{{- define "lightstack.consoleProxyDeployed" -}}
{{- if and .Values.realMode.enabled .Values.consoleProxy.enabled (ne .Values.deploymentMode "multi-node") -}}true{{- end -}}
{{- end -}}

{{- define "lightstack.config" -}}
# The binary's node-local root. Same string as the hostPath mount: libvirtd on the host
# opens the paths Lightstack writes into the domain XML.
state_path: {{ .Values.realMode.hostStoragePath | quote }}
database:
  url: {{ .Values.database.url | quote }}
  max_connections: 50
keystone:
  port: 35357
  jwt_secret: {{ .Values.secrets.jwtSecret | quote }}
  token_ttl: 24h
  admin_user: {{ .Values.secrets.adminUser | default "admin" | quote }}
  admin_password: {{ .Values.secrets.adminPassword | quote }}
  rgw_compat_tenant_ids: {{ .Values.keystone.rgwCompatTenantIds }}
compute:
  node_id: auto
  tunnel_ip: auto
  vxlan_port: {{ .Values.multiNode.vxlanPort }}
  heartbeat_interval: 30s
nova:
  port: 8774
  libvirt_uri: "qemu:///system"
  libvirt_mode: {{ .Values.nova.libvirtMode | quote }}
  console_proxy_base_url: {{ .Values.nova.consoleProxyBaseUrl | quote }}
  console_token_ttl: {{ .Values.nova.consoleTokenTtl | quote }}
  console_reconcile_interval: {{ .Values.nova.consoleReconcileInterval | quote }}
  console_enabled: {{ if include "lightstack.consoleProxyDeployed" . }}true{{ else }}false{{ end }}
  state_reconcile_interval: {{ .Values.nova.stateReconcileInterval | quote }}
  instance_action_retention: {{ .Values.nova.instanceActionRetention | quote }}
  shutdown_timeout: {{ .Values.nova.shutdownTimeout | quote }}
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
  {{- with .Values.glance.s3 }}
  s3_endpoint: {{ .endpoint | quote }}
  s3_bucket: {{ .bucket | quote }}
  s3_region: {{ .region | quote }}
  s3_ca_bundle: {{ .caBundle | quote }}
  s3_insecure_skip_verify: {{ .insecureSkipVerify }}
  {{- end }}
logging:
  level: {{ .Values.logging.level | quote }}
  format: {{ .Values.logging.format | quote }}
cache:
  enabled: {{ .Values.cache.enabled }}
  redis_url: {{ .Values.cache.redisUrl | quote }}
{{- end -}}

{{/*
LIGHTSTACK_DB_URL from database.existingSecret, for every container that opens the database:
the workload and the teardown purge Job. With the URL in a Secret, the rendered config
holds none, and `lightstack cleanup` refuses an empty URL (SPEC-028 FR-001). Renders
nothing without database.existingSecret.
*/}}
{{- define "lightstack.dbUrlEnv" -}}
{{- if .Values.database.existingSecret -}}
- name: LIGHTSTACK_DB_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: {{ .Values.database.existingSecretKey | default "url" }}
{{- end -}}
{{- end -}}

{{/*
The database backend, read from database.url's scheme (SPEC-029 FR-001). Anchored on the
`sqlite://` form the chart, docs and values use. Anything else — a URL hidden in
database.existingSecret or secrets.existingSecret, or a non-canonical single-slash
`sqlite:/path` the binary would still accept — resolves to postgres here, so the binary
catches the scheme mismatch at startup (SPEC-028 FR-004) with a clear message rather than the
chart failing the render with a confusing path error. Renders "sqlite" or "postgres".
*/}}
{{- define "lightstack.dbBackend" -}}
{{- if hasPrefix "sqlite://" .Values.database.url -}}sqlite{{- else -}}postgres{{- end -}}
{{- end -}}

{{/* Truthy only when the chart renders SQLite storage (SPEC-029). */}}
{{- define "lightstack.sqliteEnabled" -}}
{{- if eq (include "lightstack.dbBackend" .) "sqlite" -}}true{{- end -}}
{{- end -}}

{{/*
lightstack.sqliteStubMountPath — the hostPath the stub-mode SQLite workload mounts. When a local
Glance or Cinder backend writes node-local backing (storageMode contains "local"), it is the whole
state root (realMode.hostStoragePath), so images/ and volumes/ persist with the database across pod
recreation and upgrades — matching the real-mode mount and the Compose variant. Otherwise (the
stub-storage default) it is just the database directory, keeping FR-004's least-privilege mount.
*/}}
{{- define "lightstack.sqliteStubMountPath" -}}
{{- if or (contains "local" .Values.glance.storageMode) (contains "local" .Values.cinder.storageMode) -}}
{{- .Values.realMode.hostStoragePath -}}
{{- else -}}
{{- dir (trimPrefix "sqlite://" .Values.database.url) -}}
{{- end -}}
{{- end -}}

{{/*
LIGHTSTACK_DB_BACKEND for every container that runs the binary — the workload and the teardown
purge Job — so the binary can verify a URL the chart cannot see (SPEC-029 FR-003). Always set.
*/}}
{{- define "lightstack.dbBackendEnv" -}}
- name: LIGHTSTACK_DB_BACKEND
  value: {{ include "lightstack.dbBackend" . | quote }}
{{- end -}}

{{/*
Node placement — nodeSelector and tolerations — shared by every scheduling variant below. Emits
nothing when both are unset, so a default render is unchanged.
*/}}
{{- define "lightstack.nodePlacement" -}}
{{- if eq .Values.deploymentMode "multi-node" }}
{{- $kv := splitn "=" 2 .Values.multiNode.nodeSelectorLabel }}
nodeSelector:
  {{ $kv._0 }}: {{ $kv._1 | quote }}
  {{- with .Values.nodeSelector }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- else if .Values.nodeSelector }}
nodeSelector:
  {{- toYaml .Values.nodeSelector | nindent 2 }}
{{- end }}
{{- with .Values.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/*
Full pod scheduling — node placement plus the operator's whole affinity — for the workload pod
(lightstack.podSpec).
*/}}
{{- define "lightstack.scheduling" -}}
{{- include "lightstack.nodePlacement" . }}
{{- with .Values.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/*
Node-only scheduling for the teardown purge Job: node placement plus the operator's node affinity,
but NOT podAffinity/podAntiAffinity. The purge must run on the workload's node (SPEC-029 FR-010), so
it keeps the node pin — but it runs after the quiesce hook has scaled the workload to zero, so a
required pod-affinity on live workload pods would make it unschedulable and hang the uninstall. The
purge Job also deliberately omits app.kubernetes.io/name, so it would not satisfy such a term
itself. It carries no pod affinity of its own.
*/}}
{{- define "lightstack.schedulingNodeOnly" -}}
{{- include "lightstack.nodePlacement" . }}
{{- with .Values.affinity }}
{{- with .nodeAffinity }}
affinity:
  nodeAffinity:
    {{- toYaml . | nindent 4 }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Shared pod spec (spec.template.spec) for both the single-node Deployment and the
multi-node DaemonSet, so the container, config mount, probes, and real-mode host
access stay identical across modes. Include with: {{- include "lightstack.podSpec" . | nindent 6 }}
*/}}
{{- define "lightstack.podSpec" -}}
# Lightstack never calls the Kubernetes API — don't mount a ServiceAccount token (FR-009c; reduces attack surface).
automountServiceAccountToken: false
terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}
{{- with .Values.image.pullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 2 }}
{{- end }}
{{- if .Values.realMode.enabled }}
hostNetwork: {{ .Values.realMode.hostNetwork }}
{{- end }}
{{- include "lightstack.scheduling" . }}
{{- if .Values.realMode.enabled }}
initContainers:
  {{- if .Values.realMode.legacyStatePath }}
  # One-time in-place upgrade from a pre-rename install (cloud-storage#575): the
  # default node-local root moved with the product rename, so without this the upgraded
  # pod writes to the new root and stops seeing the image bases, volumes, overlays,
  # cloud-init ISOs and dnsmasq state already on the node.
  #
  # The copy loop below (`for sub in ...`) is pkg/paths/paths.go minus console-tokens;
  # init-host-storage further down still CREATES console-tokens — creating is safe,
  # copying is not. The directory is a projection of console_auth_tokens, never the
  # store of record (SPEC-021 FR-023b): Lightstack writes an entry when it issues a token
  # and afterwards only removes entries. A pre-rename node's entries predate the unix-socket
  # console and map tokens to TCP host:port targets, which libvirt may since have handed to
  # another guest — the stale-token-to-another-tenant's-guest case the unix-socket console
  # exists to prevent. Losing the projection only stops tokens resolving.
  #
  # Merges file by file. The two roots are separate volumes, so rename() returns EXDEV and
  # every move here is an interruptible copy; an interrupted one leaves a half-populated
  # directory at the destination, and a per-directory `mv -n` would then refuse that whole
  # directory on every later run, stranding the children it lacks. Nothing already at the
  # destination is overwritten, and a path present under both roots fails the container so
  # the pod does not start on top of an unfinished migration.
  - name: migrate-legacy-state
    image: {{ include "lightstack.image" . }}
    imagePullPolicy: {{ .Values.image.pullPolicy }}
    command:
      - /bin/sh
      - -c
      - >-
        set -e;
        LEGACY="{{ .Values.realMode.legacyStatePath }}";
        NEW="{{ .Values.realMode.hostStoragePath }}";
        for sub in images volumes instances cloud-init dhcp; do
        [ -d "$LEGACY/$sub" ] || continue;
        mkdir -p "$NEW/$sub";
        find "$LEGACY/$sub" -mindepth 1 -type d | while IFS= read -r d; do
        mkdir -p "$NEW/$sub/${d#"$LEGACY/$sub/"}"; done;
        find "$LEGACY/$sub" -mindepth 1 \( -type f -o -type l \) | while IFS= read -r f; do
        mv -n "$f" "$NEW/$sub/${f#"$LEGACY/$sub/"}" 2>/dev/null || true; done;
        find "$LEGACY/$sub" -mindepth 1 -type d -empty -delete 2>/dev/null || true;
        done;
        LEFT="$(find "$LEGACY" -mindepth 1 \( -type f -o -type l \) 2>/dev/null)";
        if [ -n "$LEFT" ]; then
        echo "ERROR: these paths exist under both state roots, so nothing was overwritten:";
        echo "$LEFT";
        echo "Compare each against $NEW, keep the one you want, remove the other.";
        exit 1; fi
    volumeMounts:
      - name: legacy-state
        mountPath: {{ .Values.realMode.legacyStatePath }}
      - name: host-storage
        mountPath: {{ .Values.realMode.hostStoragePath }}
  {{- end }}
  # Real-mode node-local storage: the host-storage hostPath is DirectoryOrCreate, so it
  # materializes only the base dir and shadows the image's pre-created subdirs. Pre-create
  # the per-service subdirs here (before Lightstack starts) so a fresh deploy boots a VM with no
  # manual mkdir (SPEC-011 / cloud-storage#510). Subdir list mirrors pkg/paths/paths.go.
  - name: init-host-storage
    image: {{ include "lightstack.image" . }}
    imagePullPolicy: {{ .Values.image.pullPolicy }}
    command:
      - /bin/sh
      - -c
      - >-
        mkdir -p
        "{{ .Values.realMode.hostStoragePath }}/images"
        "{{ .Values.realMode.hostStoragePath }}/volumes"
        "{{ .Values.realMode.hostStoragePath }}/instances"
        "{{ .Values.realMode.hostStoragePath }}/cloud-init"
        "{{ .Values.realMode.hostStoragePath }}/dhcp"
        "{{ .Values.realMode.hostStoragePath }}/console-tokens"
        "{{ .Values.realMode.hostStoragePath }}/console-sockets"
        "{{ .Values.realMode.hostStoragePath }}/db"
    volumeMounts:
      - name: host-storage
        mountPath: {{ .Values.realMode.hostStoragePath }}
{{- end }}
containers:
  - name: lightstack
    image: {{ include "lightstack.image" . }}
    imagePullPolicy: {{ .Values.image.pullPolicy }}
    # Full command, not args: the image declares only a CMD (no ENTRYPOINT), so bare
    # args would be exec'd directly. The binary migrates the DB itself on startup
    # (FR-012b), so the image's `lightstack-migrate &&` wrapper CMD is not needed here.
    command: ["/app/lightstack", "--config", "/app/config/lightstack.yaml"]
    {{- if .Values.realMode.enabled }}
    securityContext:
      privileged: {{ .Values.realMode.privileged }}
    {{- end }}
    env:
      # The name block-storage reservations are recorded under (SPEC-028 FR-033). Without
      # host networking the host name is the pod's name, which a recreated pod does not
      # keep; the node's name lets it find and sweep the creates it was running when it
      # stopped.
      - name: LIGHTSTACK_NODE_NAME
        valueFrom:
          fieldRef:
            fieldPath: spec.nodeName
      {{- if gt (int .Values.drainSeconds) 0 }}
      - name: LIGHTSTACK_DRAIN_SECONDS
        value: {{ .Values.drainSeconds | quote }}
      {{- end }}
      {{- with include "lightstack.dbUrlEnv" . }}
      {{- . | nindent 6 }}
      {{- end }}
      {{- include "lightstack.dbBackendEnv" . | nindent 6 }}
      {{- if .Values.endpointHost }}
      - name: LIGHTSTACK_ENDPOINT_HOST
        value: {{ .Values.endpointHost | quote }}
      {{- end }}
      {{- if .Values.tls.enabled }}
      - name: LIGHTSTACK_ENDPOINT_SCHEME
        value: "https"
      {{- end }}
      {{- with .Values.extraEnv }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    ports:
      - { name: identity, containerPort: 35357 }
      - { name: compute, containerPort: 8774 }
      - { name: placement, containerPort: 8778 }
      - { name: network, containerPort: 9696 }
      - { name: blockstorage, containerPort: 8776 }
      - { name: image, containerPort: 9292 }
      - { name: metadata, containerPort: 8775 }
    # Honest k8s probes (SPEC-013): startup+readiness gate on /readyz (DB-gated),
    # liveness on /livez (process-up; never the DB, so a DB outage never restarts).
    {{- with .Values.probes.startup }}
    {{- if .enabled }}
    startupProbe:
      httpGet: { path: /readyz, port: identity }
      periodSeconds: {{ .periodSeconds }}
      timeoutSeconds: {{ .timeoutSeconds }}
      failureThreshold: {{ .failureThreshold }}
    {{- end }}
    {{- end }}
    {{- with .Values.probes.readiness }}
    {{- if .enabled }}
    readinessProbe:
      httpGet: { path: /readyz, port: identity }
      periodSeconds: {{ .periodSeconds }}
      timeoutSeconds: {{ .timeoutSeconds }}
      failureThreshold: {{ .failureThreshold }}
      successThreshold: {{ .successThreshold }}
    {{- end }}
    {{- end }}
    {{- with .Values.probes.liveness }}
    {{- if .enabled }}
    livenessProbe:
      httpGet: { path: /livez, port: identity }
      periodSeconds: {{ .periodSeconds }}
      timeoutSeconds: {{ .timeoutSeconds }}
      failureThreshold: {{ .failureThreshold }}
    {{- end }}
    {{- end }}
    volumeMounts:
      - name: config
        mountPath: /app/config
        readOnly: true
      {{- if and (include "lightstack.sqliteEnabled" .) (not .Values.realMode.enabled) }}
      # Stub-mode SQLite: the db directory (least privilege, SPEC-029 FR-004), or the whole state
      # root when a local Glance/Cinder backend also writes node-local backing there, so images/
      # and volumes/ persist with the database. Real mode reaches it through host-storage below.
      - name: sqlite-db
        mountPath: {{ include "lightstack.sqliteStubMountPath" . }}
      {{- end }}
      {{- if .Values.realMode.enabled }}
      - name: libvirt-run
        mountPath: {{ dir .Values.realMode.libvirtSocketPath }}
      - name: host-storage
        mountPath: {{ .Values.realMode.hostStoragePath }}
      {{- end }}
    {{- with .Values.resources }}
    resources:
      {{- toYaml . | nindent 6 }}
    {{- end }}
volumes:
  - name: config
    secret:
      secretName: {{ include "lightstack.configSecretName" . }}
  {{- if and (include "lightstack.sqliteEnabled" .) (not .Values.realMode.enabled) }}
  # Stub-mode SQLite hostPath (no PVC): the db directory, or the whole state root when a local
  # Glance/Cinder backend also writes there (so the images/ and volumes/ backing persists with the
  # database). WAL keeps lightstack.db-wal and -shm beside the file, so a directory is mounted.
  - name: sqlite-db
    hostPath:
      path: {{ include "lightstack.sqliteStubMountPath" . }}
      type: DirectoryOrCreate
  {{- end }}
  {{- if .Values.realMode.enabled }}
  # The directory that holds libvirtd's socket, not the socket. A bind of the file keeps
  # the inode the pod started with, so once libvirtd re-creates its socket — its socket
  # units restart, or it runs without socket activation — the pod could not reach it
  # again, and the libvirt reconnect (pkg/hypervisor keepConnected) would fail until the
  # pod restarted. Accepted cost: the pod also sees libvirt's other sockets (read-only,
  # admin, virtlogd); it already holds the read-write one, which is root on the host.
  - name: libvirt-run
    hostPath:
      path: {{ dir .Values.realMode.libvirtSocketPath }}
      type: Directory
  - name: host-storage
    hostPath:
      path: {{ .Values.realMode.hostStoragePath }}
      type: DirectoryOrCreate
  {{- if .Values.realMode.legacyStatePath }}
  - name: legacy-state
    hostPath:
      path: {{ .Values.realMode.legacyStatePath }}
      type: DirectoryOrCreate
  {{- end }}
  {{- end }}
{{- end -}}
