{{- define "exporter.name" -}}
{{ .Release.Name }}-litellm-datahub-exporter
{{- end }}

{{- define "exporter.labels" -}}
app.kubernetes.io/name: litellm-datahub-exporter
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "exporter.selectorLabels" -}}
app.kubernetes.io/name: litellm-datahub-exporter
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "exporter.secretName" -}}
{{- if .Values.existingSecret }}{{ .Values.existingSecret }}{{ else }}{{ include "exporter.name" . }}{{ end -}}
{{- end }}
