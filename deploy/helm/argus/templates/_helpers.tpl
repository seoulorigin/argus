{{- define "argus.name" -}}argus{{- end -}}

{{- define "argus.labels" -}}
app.kubernetes.io/name: argus
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "argus.selector" -}}
app.kubernetes.io/name: argus
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "argus.secretName" -}}
{{- if .Values.auth.existingSecret -}}{{ .Values.auth.existingSecret }}{{- else -}}{{ .Release.Name }}-argus-auth{{- end -}}
{{- end -}}

{{- define "argus.hubName" -}}{{ .Release.Name }}-argus-hub{{- end -}}
{{- define "argus.agentName" -}}{{ .Release.Name }}-argus-agent{{- end -}}
