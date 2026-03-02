{{/*
Construct the DATABASE_URL from values.
- Internal PG: auto-construct from postgresql.auth values
- External (url): use externalDatabase.url as-is
- External (fields): construct from individual externalDatabase fields
*/}}
{{- define "agentserver.databaseUrl" -}}
{{- if .Values.postgresql.enabled -}}
postgres://{{ .Values.postgresql.auth.username }}:{{ .Values.postgresql.auth.password }}@{{ .Release.Name }}-postgresql:5432/{{ .Values.postgresql.auth.database }}?sslmode=disable
{{- else if .Values.externalDatabase.url -}}
{{ .Values.externalDatabase.url }}
{{- else if .Values.externalDatabase.host -}}
postgres://{{ .Values.externalDatabase.user }}:{{ .Values.externalDatabase.password }}@{{ .Values.externalDatabase.host }}:{{ .Values.externalDatabase.port }}/{{ .Values.externalDatabase.database }}?sslmode={{ .Values.externalDatabase.sslmode }}
{{- end -}}
{{- end -}}

{{/*
Return the secret name containing the database-url key.
- If externalDatabase.existingSecret is set, use that
- Otherwise, use the chart-managed secret
*/}}
{{- define "agentserver.databaseSecretName" -}}
{{- if .Values.externalDatabase.existingSecret -}}
{{ .Values.externalDatabase.existingSecret }}
{{- else -}}
{{ .Release.Name }}-secret
{{- end -}}
{{- end -}}
