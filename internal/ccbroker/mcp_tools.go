package ccbroker

func buildToolDefinitions() []MCPToolDef {
	return []MCPToolDef{
		{
			Name:        "remote_bash",
			Description: "Execute a shell command on a remote executor. Use list_executors to discover available executors and obtain executor_id.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"executor_id": map[string]interface{}{
						"type":        "string",
						"description": "Target executor ID. Use list_executors to discover available executors.",
					},
					"command": map[string]interface{}{
						"type":        "string",
						"description": "Shell command to execute on the remote executor.",
					},
					"timeout": map[string]interface{}{
						"type":        "integer",
						"description": "Optional timeout in milliseconds for the command execution.",
					},
					"description": map[string]interface{}{
						"type":        "string",
						"description": "Optional human-readable description of what this command does.",
					},
				},
				"required": []string{"executor_id", "command"},
			},
		},
		{
			Name:        "remote_read",
			Description: "Read the contents of a file on a remote executor. Use list_executors to discover available executors and obtain executor_id.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"executor_id": map[string]interface{}{
						"type":        "string",
						"description": "Target executor ID. Use list_executors to discover available executors.",
					},
					"file_path": map[string]interface{}{
						"type":        "string",
						"description": "Absolute path to the file to read on the remote executor.",
					},
					"offset": map[string]interface{}{
						"type":        "integer",
						"description": "Optional line number offset to start reading from (0-based).",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional maximum number of lines to read.",
					},
				},
				"required": []string{"executor_id", "file_path"},
			},
		},
		{
			Name:        "remote_edit",
			Description: "Edit a file on a remote executor by replacing a specific string with a new string. Use list_executors to discover available executors and obtain executor_id.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"executor_id": map[string]interface{}{
						"type":        "string",
						"description": "Target executor ID. Use list_executors to discover available executors.",
					},
					"file_path": map[string]interface{}{
						"type":        "string",
						"description": "Absolute path to the file to edit on the remote executor.",
					},
					"old_string": map[string]interface{}{
						"type":        "string",
						"description": "The exact string to find and replace in the file.",
					},
					"new_string": map[string]interface{}{
						"type":        "string",
						"description": "The replacement string to substitute for old_string.",
					},
					"replace_all": map[string]interface{}{
						"type":        "boolean",
						"description": "If true, replace all occurrences of old_string. Defaults to false (replace only the first occurrence).",
					},
				},
				"required": []string{"executor_id", "file_path", "old_string", "new_string"},
			},
		},
		{
			Name:        "remote_write",
			Description: "Write content to a file on a remote executor, creating or overwriting it. Use list_executors to discover available executors and obtain executor_id.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"executor_id": map[string]interface{}{
						"type":        "string",
						"description": "Target executor ID. Use list_executors to discover available executors.",
					},
					"file_path": map[string]interface{}{
						"type":        "string",
						"description": "Absolute path to the file to write on the remote executor.",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "The content to write to the file.",
					},
				},
				"required": []string{"executor_id", "file_path", "content"},
			},
		},
		{
			Name:        "remote_glob",
			Description: "Find files matching a glob pattern on a remote executor. Use list_executors to discover available executors and obtain executor_id.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"executor_id": map[string]interface{}{
						"type":        "string",
						"description": "Target executor ID. Use list_executors to discover available executors.",
					},
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "Glob pattern to match files against (e.g. '**/*.go', 'src/**/*.ts').",
					},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Optional directory path to search within. Defaults to the current working directory.",
					},
				},
				"required": []string{"executor_id", "pattern"},
			},
		},
		{
			Name:        "remote_grep",
			Description: "Search for a pattern in files on a remote executor using ripgrep. Use list_executors to discover available executors and obtain executor_id.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"executor_id": map[string]interface{}{
						"type":        "string",
						"description": "Target executor ID. Use list_executors to discover available executors.",
					},
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "Regular expression pattern to search for in file contents.",
					},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Optional file or directory path to search in. Defaults to the current working directory.",
					},
					"glob": map[string]interface{}{
						"type":        "string",
						"description": "Optional glob pattern to filter files (e.g. '*.go', '**/*.ts').",
					},
				},
				"required": []string{"executor_id", "pattern"},
			},
		},
		{
			Name:        "remote_ls",
			Description: "List directory contents on a remote executor. Use list_executors to discover available executors and obtain executor_id.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"executor_id": map[string]interface{}{
						"type":        "string",
						"description": "Target executor ID. Use list_executors to discover available executors.",
					},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Optional directory path to list. Defaults to the current working directory.",
					},
				},
				"required": []string{"executor_id"},
			},
		},
		{
			Name:        "list_executors",
			Description: "List available remote executors that can be used with remote_* tools. Returns executor IDs, status, and metadata.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"status_filter": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"online", "all"},
						"description": "Filter executors by status. 'online' returns only currently connected executors (default). 'all' returns all executors including offline ones.",
						"default":     "online",
					},
				},
			},
		},
		{
			Name:        "workspace_write",
			Description: "Write content to a file in the broker's shared workspace. The workspace is accessible across sessions and can be used to share data between tools.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Relative path within the workspace where the file should be written.",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "The content to write to the workspace file.",
					},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "workspace_read",
			Description: "Read the contents of a file from the broker's shared workspace.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Relative path within the workspace of the file to read.",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "workspace_ls",
			Description: "List files and directories in the broker's shared workspace.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Optional relative path within the workspace to list. Defaults to the workspace root.",
						"default":     "",
					},
				},
			},
		},
		{
			Name:        "send_message",
			Description: "Send a text message to the human operator monitoring this agent session.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"text": map[string]interface{}{
						"type":        "string",
						"description": "The text message to send to the operator.",
					},
					"sender": map[string]interface{}{
						"type":        "string",
						"description": "Optional display name identifying who is sending the message.",
					},
				},
				"required": []string{"text"},
			},
		},
		{
			Name:        "send_image",
			Description: "Send an image to the human operator monitoring this agent session.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"source": map[string]interface{}{
						"type":        "string",
						"description": "The image source — either a URL or a base64-encoded data string.",
					},
					"format": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"png", "jpeg", "gif", "webp"},
						"description": "Optional image format. Defaults to png if not specified.",
					},
					"caption": map[string]interface{}{
						"type":        "string",
						"description": "Optional caption text to accompany the image.",
					},
				},
				"required": []string{"source"},
			},
		},
		{
			Name:        "send_file",
			Description: "Send a file to the human operator monitoring this agent session.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"source": map[string]interface{}{
						"type":        "string",
						"description": "The file source — either a URL or a base64-encoded data string.",
					},
					"filename": map[string]interface{}{
						"type":        "string",
						"description": "The filename to use when presenting the file to the operator.",
					},
					"caption": map[string]interface{}{
						"type":        "string",
						"description": "Optional caption or description text to accompany the file.",
					},
				},
				"required": []string{"source", "filename"},
			},
		},
		{
			Name:        "create_scheduled_task",
			Description: "Schedule a prompt to run on a cron schedule. The scheduled task will invoke the agent with the given prompt at each scheduled time.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"cron": map[string]interface{}{
						"type":        "string",
						"description": "Standard 5-field cron expression specifying when to run (e.g. '0 9 * * 1-5' for weekdays at 9am, '*/15 * * * *' for every 15 minutes).",
					},
					"prompt": map[string]interface{}{
						"type":        "string",
						"description": "The prompt text to send to the agent when the scheduled task fires.",
					},
					"recurring": map[string]interface{}{
						"type":        "boolean",
						"description": "If true (default), the task repeats on every cron match. If false, it fires once and is automatically removed.",
						"default":     true,
					},
					"description": map[string]interface{}{
						"type":        "string",
						"description": "Optional human-readable description of what this scheduled task does.",
					},
				},
				"required": []string{"cron", "prompt"},
			},
		},
		{
			Name:        "list_scheduled_tasks",
			Description: "List all currently scheduled tasks managed by the broker.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "cancel_scheduled_task",
			Description: "Cancel and remove a scheduled task by its ID. Use list_scheduled_tasks to find task IDs.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": map[string]interface{}{
						"type":        "string",
						"description": "The ID of the scheduled task to cancel. Use list_scheduled_tasks to discover task IDs.",
					},
				},
				"required": []string{"task_id"},
			},
		},
	}
}
