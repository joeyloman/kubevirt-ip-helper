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
app.kubernetes.io/name: {{ include "kubevirt-ip-helper.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "kubevirt-ip-helper.serviceAccountName" -}}
{{- if .Values.kubevirtiphelper.serviceAccount.create }}
{{- default (include "kubevirt-ip-helper.fullname" .) .Values.kubevirtiphelper.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.kubevirtiphelper.serviceAccount.name }}
{{- end }}
{{- end }}
