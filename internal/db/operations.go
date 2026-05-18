package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Operation is one row of the operations table — a single completed
// mcpServer/tool/call observed by codex-app-gateway.
type Operation struct {
	ID          string
	WorkspaceID string
	UserID      *string
	Source      string // "sdk" | "tui" | (v1.5) "llm"
	ThreadID    *string
	RequestID   *string

	EnvID         string
	Tool          string
	Arguments     json.RawMessage // nil if truncated
	ArgumentsMeta json.RawMessage // {"truncated":true,"size_bytes":N,"sha256":"..."} if so

	IsError       bool
	ResultSummary *string
	ResultMeta    json.RawMessage

	StartedAt   time.Time
	CompletedAt time.Time
	DurationMs  int32

	NotebookPath *string // v1.5
	CellID       *string // v1.5
}

// OperationFilter is the optional filter set for ListOperations.
type OperationFilter struct {
	WorkspaceID string // REQUIRED — server-side enforced
	EnvID       string // optional
	Tool        string // optional
	Source      string // optional
	IsError     *bool  // optional
	Since       *time.Time
	ID          string // optional: exact match (returns 0 or 1 rows)
	Limit       int    // default 100, max 1000
}

func (db *DB) InsertOperation(o Operation) error {
	const q = `
INSERT INTO operations (
  id, workspace_id, user_id, source, thread_id, request_id,
  env_id, tool, arguments, arguments_meta,
  is_error, result_summary, result_meta,
  started_at, completed_at, duration_ms,
  notebook_path, cell_id
) VALUES (
  $1,$2,$3,$4,$5,$6, $7,$8,$9,$10, $11,$12,$13, $14,$15,$16, $17,$18
)`
	_, err := db.Exec(q,
		o.ID, o.WorkspaceID, o.UserID, o.Source, o.ThreadID, o.RequestID,
		o.EnvID, o.Tool, nullableJSON(o.Arguments), nullableJSON(o.ArgumentsMeta),
		o.IsError, o.ResultSummary, nullableJSON(o.ResultMeta),
		o.StartedAt, o.CompletedAt, o.DurationMs,
		o.NotebookPath, o.CellID,
	)
	return err
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

const defaultListLimit = 100
const maxListLimit = 1000

func (db *DB) ListOperations(f OperationFilter) ([]Operation, error) {
	if f.WorkspaceID == "" {
		return nil, fmt.Errorf("ListOperations: WorkspaceID required")
	}
	if f.Limit <= 0 {
		f.Limit = defaultListLimit
	}
	if f.Limit > maxListLimit {
		f.Limit = maxListLimit
	}

	var (
		args  []any
		where []string
	)
	pushArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where = append(where, "workspace_id = "+pushArg(f.WorkspaceID))
	if f.ID != "" {
		where = append(where, "id = "+pushArg(f.ID))
	}
	if f.EnvID != "" {
		where = append(where, "env_id = "+pushArg(f.EnvID))
	}
	if f.Tool != "" {
		where = append(where, "tool = "+pushArg(f.Tool))
	}
	if f.Source != "" {
		where = append(where, "source = "+pushArg(f.Source))
	}
	if f.IsError != nil {
		where = append(where, "is_error = "+pushArg(*f.IsError))
	}
	if f.Since != nil {
		where = append(where, "started_at >= "+pushArg(*f.Since))
	}
	limit := pushArg(f.Limit)

	q := `SELECT id, workspace_id, user_id, source, thread_id, request_id,
		env_id, tool, arguments, arguments_meta,
		is_error, result_summary, result_meta,
		started_at, completed_at, duration_ms,
		notebook_path, cell_id
		FROM operations WHERE ` + strings.Join(where, " AND ") +
		" ORDER BY started_at DESC LIMIT " + limit
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		var o Operation
		var argsCol, argsMeta, resMeta sql.NullString
		err := rows.Scan(
			&o.ID, &o.WorkspaceID, &o.UserID, &o.Source, &o.ThreadID, &o.RequestID,
			&o.EnvID, &o.Tool, &argsCol, &argsMeta,
			&o.IsError, &o.ResultSummary, &resMeta,
			&o.StartedAt, &o.CompletedAt, &o.DurationMs,
			&o.NotebookPath, &o.CellID,
		)
		if err != nil {
			return nil, err
		}
		if argsCol.Valid {
			o.Arguments = json.RawMessage(argsCol.String)
		}
		if argsMeta.Valid {
			o.ArgumentsMeta = json.RawMessage(argsMeta.String)
		}
		if resMeta.Valid {
			o.ResultMeta = json.RawMessage(resMeta.String)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PruneOperationsOlderThan deletes operations whose started_at is strictly
// before cutoff. Returns the number of rows deleted.
func (db *DB) PruneOperationsOlderThan(cutoff time.Time) (int64, error) {
	res, err := db.Exec("DELETE FROM operations WHERE started_at < $1", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
