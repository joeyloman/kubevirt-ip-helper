{{/*
Expand the name of the chart.
*/}}
{{- define "kubevirt-ip-helper.name" -}}
{{- default .Chart.Name .Values.kubevirtiphelper.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "kubevirt-ip-helper.fullname" -}}
{{- if .Values.kubevirtiphelper.fullnameOverride }}
{{- .Values.kubevirtiphelper.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.kubevirtiphelper.nameOverride }}
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
{{- define "kubevirt-ip-helper.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kubevirt-ip-helper.labels" -}}
helm.sh/chart: {{ include "kubevirt-ip-helper.chart" . }}
{{ include "kubevirt-ip-helper.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kubevirt-ip-helper.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubevirt-ip-helper.name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "kubevirt-ip-helper.serviceAccountName" -}}
{{- if .Values.kubevirtiphelper.serviceAccount.create }}
{{- default (include "kubevirt-ip-helper.fullname" .) .Values.kubevirtiphelper.serviceAccount.name }}
{{- else }}
{{- required "kubevirtiphelper.serviceAccount.name is required when create is false" .Values.kubevirtiphelper.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "kubevirt-ip-helper.dnsLabel" -}}
{{- if not (kindIs "string" .value) -}}
{{- fail (printf "%s must be a nonempty DNS label of at most 63 characters" .field) -}}
{{- end -}}
{{- if or (gt (len .value) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .value)) -}}
{{- fail (printf "%s must be a nonempty DNS label of at most 63 characters" .field) -}}
{{- end -}}
{{- end -}}

{{- define "kubevirt-ip-helper.validate" -}}
{{- if ne .Release.Namespace "kubevirt-ip-helper" -}}
{{- fail "install one release in namespace kubevirt-ip-helper" -}}
{{- end -}}
{{- if ne .Values.webhook.fullnameOverride "kubevirt-ip-helper-webhook" -}}
{{- fail "webhook.fullnameOverride must be kubevirt-ip-helper-webhook (runtime TLS/admission identity)" -}}
{{- end -}}
{{- if or (ne (toString .Values.webhook.service.webhookServicePort) "8080") (ne (toString .Values.webhook.service.webhookListenPort) "8443") -}}
{{- fail "webhook service ports must remain 8080 -> 8443" -}}
{{- end -}}
{{- $helperName := include "kubevirt-ip-helper.name" . -}}
{{- $helperFullname := include "kubevirt-ip-helper.fullname" . -}}
{{- $webhookName := include "kubevirt-ip-helper-webhook.name" . -}}
{{- $webhookFullname := include "kubevirt-ip-helper-webhook.fullname" . -}}
{{- if eq $helperName $webhookName -}}
{{- fail "helper and webhook selector names must differ" -}}
{{- end -}}
{{- if eq $helperFullname $webhookFullname -}}
{{- fail "helper and webhook shared RBAC names must differ" -}}
{{- end -}}
{{- $helperAccount := include "kubevirt-ip-helper.serviceAccountName" . -}}
{{- $webhookAccount := include "kubevirt-ip-helper-webhook.serviceAccountName" . -}}
{{- if and (eq $helperAccount $webhookAccount) .Values.kubevirtiphelper.serviceAccount.create .Values.webhook.serviceAccount.create -}}
{{- fail "helper and webhook cannot both create the same ServiceAccount" -}}
{{- end -}}
{{- if not (kindIs "slice" .Values.kubevirtiphelper.networks) -}}
{{- fail "kubevirtiphelper.networks must be a nonempty list" -}}
{{- end -}}
{{- if not .Values.kubevirtiphelper.networks -}}
{{- fail "kubevirtiphelper.networks must be a nonempty list" -}}
{{- end -}}
{{- $nads := dict -}}
{{- $deployments := dict $webhookFullname true -}}
{{- $services := dict $webhookFullname true -}}
{{- range $i, $network := .Values.kubevirtiphelper.networks -}}
{{- if not (kindIs "map" $network) -}}
{{- fail (printf "kubevirtiphelper.networks[%d] must be a mapping" $i) -}}
{{- end -}}
{{- range $field := list "name" "deploymentName" "metricsServiceName" -}}
{{- include "kubevirt-ip-helper.dnsLabel" (dict "field" (printf "networks[%d].%s" $i $field) "value" (get $network $field)) -}}
{{- end -}}
{{- if not (regexMatch "^[a-z]" $network.metricsServiceName) -}}
{{- fail (printf "networks[%d].metricsServiceName must start with a letter (Kubernetes Service DNS label)" $i) -}}
{{- end -}}
{{- if hasKey $nads $network.name -}}
{{- fail (printf "duplicate NAD name %s" $network.name) -}}
{{- end -}}
{{- if hasKey $deployments $network.deploymentName -}}
{{- fail (printf "Deployment name %s collides with another network or shared resource" $network.deploymentName) -}}
{{- end -}}
{{- if hasKey $services $network.metricsServiceName -}}
{{- fail (printf "Service name %s collides with another network or shared resource" $network.metricsServiceName) -}}
{{- end -}}
{{- $_ := set $nads $network.name true -}}
{{- $_ := set $deployments $network.deploymentName true -}}
{{- $_ := set $services $network.metricsServiceName true -}}
{{- $interface := required (printf "networks[%d].interface is required" $i) $network.interface -}}
{{- if not (kindIs "string" $interface) -}}
{{- fail (printf "networks[%d].interface must be a string" $i) -}}
{{- end -}}
{{- if hasKey $network "replicaCount" -}}
{{- if not (regexMatch "^(0|[1-9][0-9]*)$" (toString $network.replicaCount)) -}}
{{- fail (printf "networks[%d].replicaCount must be a nonnegative integer" $i) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
