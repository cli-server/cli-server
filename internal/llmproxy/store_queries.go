package llmproxy

import (
	"database/sql"
	"fmt"
	"strings"
)

// GetOrCreateTrace returns an existing trace or creates a new one.
// Uses INSERT ... ON CONFLICT to avoid TOCTOU races under concurrent requests.
func (s *Store) GetOrCreateTrace(traceID, sandboxID, workspaceID, source string) (*Trace, error) {
	t := &Trace{}
	err := s.db.QueryRow(
		`INSERT INTO traces (id, sandbox_id, workspace_id, source)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (id) DO UPDATE SET updated_at = NOW()
		 RETURNING id, sandbox_id, workspace_id, source, created_at, updated_at`,
		traceID, sandboxID, workspaceID, source,
	).Scan(&t.ID, &t.SandboxID, &t.WorkspaceID, &t.Source, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert trace: %w", err)
	}
	return t, nil
}

// UpdateTraceActivity updates the updated_at timestamp on a trace.
func (s *Store) UpdateTraceActivity(traceID string) error {
	_, err := s.db.Exec(`UPDATE traces SET updated_at = NOW() WHERE id = $1`, traceID)
	if err != nil {
		return fmt.Errorf("update trace activity: %w", err)
	}
	return nil
}

// RecordUsage inserts a single API request usage record.
func (s *Store) RecordUsage(u TokenUsage) error {
	_, err := s.db.Exec(
		`INSERT INTO usage (id, trace_id, sandbox_id, workspace_id, provider, model, message_id,
			input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
			streaming, duration_ms, ttft_ms, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		u.ID, nullIfEmpty(u.TraceID), u.SandboxID, u.WorkspaceID, u.Provider, u.Model,
		nullIfEmpty(u.MessageID), u.InputTokens, u.OutputTokens,
		u.CacheCreationInputTokens, u.CacheReadInputTokens,
		u.Streaming, u.DurationMs, u.TTFTMs, u.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("record usage: %w", err)
	}
	return nil
}

// QueryUsage returns aggregated usage grouped by provider and model.
func (s *Store) QueryUsage(opts QueryOpts) ([]UsageSummary, error) {
	var conditions []string
	var args []interface{}
	argN := 1

	if opts.WorkspaceID != "" {
		conditions = append(conditions, fmt.Sprintf("workspace_id = $%d", argN))
		args = append(args, opts.WorkspaceID)
		argN++
	}
	if opts.SandboxID != "" {
		conditions = append(conditions, fmt.Sprintf("sandbox_id = $%d", argN))
		args = append(args, opts.SandboxID)
		argN++
	}
	if !opts.Since.IsZero() {
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", argN))
		args = append(args, opts.Since)
		argN++
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT provider, model,
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_creation_input_tokens), 0),
			COALESCE(SUM(cache_read_input_tokens), 0),
			COUNT(*)
		FROM usage %s
		GROUP BY provider, model
		ORDER BY provider, model`, where)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer rows.Close()

	var results []UsageSummary
	for rows.Next() {
		var u UsageSummary
		if err := rows.Scan(&u.Provider, &u.Model, &u.InputTokens, &u.OutputTokens,
			&u.CacheCreationInputTokens, &u.CacheReadInputTokens, &u.RequestCount); err != nil {
			return nil, fmt.Errorf("scan usage: %w", err)
		}
		results = append(results, u)
	}
	return results, rows.Err()
}

// QueryTraces returns traces with aggregated statistics.
func (s *Store) QueryTraces(opts QueryOpts) ([]TraceWithStats, error) {
	var conditions []string
	var args []interface{}
	argN := 1

	if opts.WorkspaceID != "" {
		conditions = append(conditions, fmt.Sprintf("t.workspace_id = $%d", argN))
		args = append(args, opts.WorkspaceID)
		argN++
	}
	if opts.SandboxID != "" {
		conditions = append(conditions, fmt.Sprintf("t.sandbox_id = $%d", argN))
		args = append(args, opts.SandboxID)
		argN++
	}
	if !opts.Since.IsZero() {
		conditions = append(conditions, fmt.Sprintf("t.created_at >= $%d", argN))
		args = append(args, opts.Since)
		argN++
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	limit := 100
	if opts.Limit > 0 && opts.Limit < 1000 {
		limit = opts.Limit
	}

	query := fmt.Sprintf(`
		SELECT t.id, t.sandbox_id, t.workspace_id, t.source, t.created_at, t.updated_at,
			COALESCE(COUNT(u.id), 0),
			COALESCE(SUM(u.input_tokens), 0),
			COALESCE(SUM(u.output_tokens), 0)
		FROM traces t
		LEFT JOIN usage u ON u.trace_id = t.id
		%s
		GROUP BY t.id
		ORDER BY t.updated_at DESC
		LIMIT %d`, where, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query traces: %w", err)
	}
	defer rows.Close()

	var results []TraceWithStats
	for rows.Next() {
		var ts TraceWithStats
		if err := rows.Scan(&ts.ID, &ts.SandboxID, &ts.WorkspaceID, &ts.Source,
			&ts.CreatedAt, &ts.UpdatedAt, &ts.RequestCount,
			&ts.TotalInputTokens, &ts.TotalOutputTokens); err != nil {
			return nil, fmt.Errorf("scan trace: %w", err)
		}
		results = append(results, ts)
	}
	return results, rows.Err()
}

// GetTraceDetail returns a trace and all its usage records.
func (s *Store) GetTraceDetail(traceID string) (*Trace, []TokenUsage, error) {
	t := &Trace{}
	err := s.db.QueryRow(
		`SELECT id, sandbox_id, workspace_id, source, created_at, updated_at FROM traces WHERE id = $1`,
		traceID,
	).Scan(&t.ID, &t.SandboxID, &t.WorkspaceID, &t.Source, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get trace detail: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT id, COALESCE(trace_id, ''), sandbox_id, workspace_id, provider, model,
			COALESCE(message_id, ''), input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			streaming, duration_ms, ttft_ms, created_at
		 FROM usage WHERE trace_id = $1 ORDER BY created_at ASC`,
		traceID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("get trace requests: %w", err)
	}
	defer rows.Close()

	var usages []TokenUsage
	for rows.Next() {
		var u TokenUsage
		if err := rows.Scan(&u.ID, &u.TraceID, &u.SandboxID, &u.WorkspaceID,
			&u.Provider, &u.Model, &u.MessageID, &u.InputTokens, &u.OutputTokens,
			&u.CacheCreationInputTokens, &u.CacheReadInputTokens,
			&u.Streaming, &u.DurationMs, &u.TTFTMs, &u.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan usage: %w", err)
		}
		usages = append(usages, u)
	}
	return t, usages, rows.Err()
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
