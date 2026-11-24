{{/*
Name of every resource in the chart. It is the release name, or
<release>-config-reload-operator when the release name does not already
contain the chart name.
*/}}
{{- define "config-reload-operator.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Labels that pick the controller Pods. They go into the Deployment selector,
which cannot be changed later, so keep them to these two.
*/}}
{{- define "config-reload-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Labels that pick the webhook Pods. The name is another one than for the
controller Pods, so neither Deployment selects the Pods of the other.
*/}}
{{- define "config-reload-operator.webhookSelectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}-webhook
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Labels for every resource.
*/}}
{{- define "config-reload-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "config-reload-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
