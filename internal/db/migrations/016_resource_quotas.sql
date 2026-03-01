-- Per-user resource overrides
ALTER TABLE user_quotas ADD COLUMN workspace_drive_size TEXT;
ALTER TABLE user_quotas ADD COLUMN sandbox_cpu TEXT;
ALTER TABLE user_quotas ADD COLUMN sandbox_memory TEXT;
ALTER TABLE user_quotas ADD COLUMN idle_timeout TEXT;
ALTER TABLE user_quotas ADD COLUMN ws_max_total_cpu TEXT;
ALTER TABLE user_quotas ADD COLUMN ws_max_total_memory TEXT;
ALTER TABLE user_quotas ADD COLUMN ws_max_idle_timeout TEXT;

-- Track allocated resources per sandbox for workspace budget enforcement
ALTER TABLE sandboxes ADD COLUMN cpu_millicores INTEGER;
ALTER TABLE sandboxes ADD COLUMN memory_bytes BIGINT;
