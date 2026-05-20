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
{{/*
Construct the Hydra DSN.
- Shared PG: use the same PostgreSQL instance with a different database name
- External: use hydra.database.externalDsn
*/}}
{{- define "agentserver.hydraDsn" -}}
{{- if .Values.hydra.database.externalDsn -}}
{{ .Values.hydra.database.externalDsn }}
{{- else -}}
postgres://{{ .Values.postgresql.auth.username }}:{{ .Values.postgresql.auth.password }}@{{ .Release.Name }}-postgresql:5432/{{ .Values.hydra.database.name }}?sslmode=disable
{{- end -}}
{{- end -}}

{{- define "agentserver.databaseSecretName" -}}
{{- if .Values.externalDatabase.existingSecret -}}
{{ .Values.externalDatabase.existingSecret }}
{{- else -}}
{{ .Release.Name }}-secret
{{- end -}}
{{- end -}}

{{/*
Construct the codex-exec-gateway DATABASE_URL: shared PG by default,
override via externalUrl.
*/}}
{{- define "agentserver.codexExecGatewayDatabaseUrl" -}}
{{- $extUrl := (dig "database" "externalUrl" "" .Values.codexExecGateway) -}}
{{- $dbName := (dig "database" "name" "codexexecgateway" .Values.codexExecGateway) -}}
{{- if $extUrl -}}
{{ $extUrl }}
{{- else -}}
postgres://{{ .Values.postgresql.auth.username }}:{{ .Values.postgresql.auth.password }}@{{ .Release.Name }}-postgresql:5432/{{ $dbName }}?sslmode=disable
{{- end -}}
{{- end -}}
