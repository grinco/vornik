{{/*
_helpers.tpl — reusable template bits.

Keep these small and named so templates stay readable; the recommended
Helm convention is chart.* helpers that return strings suitable for
labels, selectors, and resource names.
*/}}

{{- define "vornik.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
fullname: the prefix used for every resource this chart creates. Follows
the "<release>-<chart>" convention so multiple releases in one namespace
don't collide. 63-char limit matches k8s label/DNS constraints.
*/}}
{{- define "vornik.fullname" -}}
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
{{- end }}

{{- define "vornik.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Common labels for every object: stable across upgrades so selectors
don't break. app.kubernetes.io/version tracks Chart.AppVersion, not the
chart version itself — that's the vornik binary version.
*/}}
{{- define "vornik.labels" -}}
helm.sh/chart: {{ include "vornik.chart" . }}
{{ include "vornik.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
vornik.managed: "true"
{{- end }}

{{/*
Selector labels — the minimal set that identifies pods belonging to
this release. Never add fields here that change across upgrades
(version, revision) or the StatefulSet selector becomes immutable.
*/}}
{{- define "vornik.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vornik.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Per-component fullnames. Keeping them in _helpers.tpl means we can
rename the release and nothing else needs touching.
*/}}
{{- define "vornik.postgresFullname" -}}
{{- printf "%s-postgres" (include "vornik.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "vornik.postgresSelectorLabels" -}}
app.kubernetes.io/name: {{ include "vornik.name" . }}-postgres
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: postgres
{{- end }}

{{- define "vornik.postgresLabels" -}}
helm.sh/chart: {{ include "vornik.chart" . }}
{{ include "vornik.postgresSelectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
vornik.managed: "true"
{{- end }}

{{/*
ServiceAccount name resolution: use the one set in values, else derive
from the release. Centralised so both the SA object and the pod spec
read the same source.
*/}}
{{- define "vornik.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "vornik.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{/*
Secret name resolution: prefer the user-supplied existing secret;
otherwise the one this chart creates.
*/}}
{{- define "vornik.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- include "vornik.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
Database host: when the bundled postgres is enabled we point vornik at
the in-cluster service; otherwise fall through to values.database.host.
Fails loudly if neither is set — silent misconfiguration is worse than
a render-time error.
*/}}
{{- define "vornik.databaseHost" -}}
{{- if .Values.postgres.enabled -}}
{{- include "vornik.postgresFullname" . -}}
{{- else -}}
{{- required "database.host is required when postgres.enabled=false" .Values.database.host -}}
{{- end -}}
{{- end }}

{{/*
Image reference helpers. Tag falls back to Chart.AppVersion when unset
so operators don't have to bump two values on every release.
*/}}
{{- define "vornik.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{- define "vornik.agentImage" -}}
{{- printf "%s:%s" .Values.agentImage.repository .Values.agentImage.tag -}}
{{- end }}

{{- define "vornik.postgresImage" -}}
{{- printf "%s:%s" .Values.postgres.image.repository .Values.postgres.image.tag -}}
{{- end }}

{{/*
Thin image reference (podman-free; used by ui and webhook tiers in cluster
mode). Repository falls back to image.repository; tag falls back to
<appVersion>-thin.
*/}}
{{- define "vornik.thinImage" -}}
{{- $repo := default .Values.image.repository .Values.cluster.thinImage.repository -}}
{{- $appTag := printf "%s-thin" .Chart.AppVersion -}}
{{- $tag := default $appTag .Values.cluster.thinImage.tag -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end }}

{{/*
Per-role fullnames for cluster mode.
*/}}
{{- define "vornik.workerFullname" -}}
{{- printf "%s-worker" (include "vornik.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "vornik.uiFullname" -}}
{{- printf "%s-ui" (include "vornik.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "vornik.webhookFullname" -}}
{{- printf "%s-webhook" (include "vornik.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Per-role selector labels (stable across upgrades; never include version).
*/}}
{{- define "vornik.workerSelectorLabels" -}}
app.kubernetes.io/name: {{ include "vornik.name" . }}-worker
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: worker
{{- end }}

{{- define "vornik.uiSelectorLabels" -}}
app.kubernetes.io/name: {{ include "vornik.name" . }}-ui
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: ui
{{- end }}

{{- define "vornik.webhookSelectorLabels" -}}
app.kubernetes.io/name: {{ include "vornik.name" . }}-webhook
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: webhook
{{- end }}

{{/*
Per-role full labels (selector labels + chart/version/managed-by).
*/}}
{{- define "vornik.workerLabels" -}}
helm.sh/chart: {{ include "vornik.chart" . }}
{{ include "vornik.workerSelectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
vornik.managed: "true"
{{- end }}

{{- define "vornik.uiLabels" -}}
helm.sh/chart: {{ include "vornik.chart" . }}
{{ include "vornik.uiSelectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
vornik.managed: "true"
{{- end }}

{{- define "vornik.webhookLabels" -}}
helm.sh/chart: {{ include "vornik.chart" . }}
{{ include "vornik.webhookSelectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
vornik.managed: "true"
{{- end }}

{{/*
Relay upstream for the webhook tier. Defaults to the worker service DNS
on the relay port when not explicitly set.
*/}}
{{- define "vornik.relayUpstream" -}}
{{- if .Values.cluster.webhook.relayUpstream -}}
{{- .Values.cluster.webhook.relayUpstream -}}
{{- else -}}
{{- printf "https://%s:8443" (include "vornik.workerFullname" .) -}}
{{- end -}}
{{- end }}

{{/*
mTLS validation helper: fail loudly when cluster.enabled + worker and
webhook are both enabled but no mtls.secretName is provided.
Produces no output on success — call with include only for side-effects.
*/}}
{{- define "vornik.validateClusterMtls" -}}
{{- if and .Values.cluster.enabled .Values.cluster.worker.enabled .Values.cluster.webhook.enabled -}}
{{- if not .Values.cluster.mtls.secretName -}}
{{- fail "cluster.mtls.secretName is required when cluster.enabled=true with both worker and webhook enabled. See the chart README for the expected Secret keys (ca.crt, server.crt, server.key, client.crt, client.key)." -}}
{{- end -}}
{{- end -}}
{{- end }}
