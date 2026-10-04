{{- define "kube-ups-taint.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kube-ups-taint.fullname" -}}
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

{{- define "kube-ups-taint.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kube-ups-taint.labels" -}}
helm.sh/chart: {{ include "kube-ups-taint.chart" . }}
{{ include "kube-ups-taint.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "kube-ups-taint.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kube-ups-taint.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kube-ups-taint.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kube-ups-taint.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "kube-ups-taint.cidr" -}}
{{- $host := . | toString | trimPrefix "[" | trimSuffix "]" -}}
{{- if contains "/" $host -}}
{{- $host -}}
{{- else if contains ":" $host -}}
{{- printf "%s/128" $host -}}
{{- else -}}
{{- printf "%s/32" $host -}}
{{- end -}}
{{- end -}}

{{- define "kube-ups-taint.imageTag" -}}
{{- if .Values.image.tag -}}
    {{- .Values.image.tag -}}
{{- else -}}
    {{ .Chart.AppVersion }}
{{- end -}}
{{- end -}}
