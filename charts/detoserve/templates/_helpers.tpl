{{- define "detoserve.labels" -}}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: detoserve
{{- end -}}

{{- define "detoserve.selectorLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
