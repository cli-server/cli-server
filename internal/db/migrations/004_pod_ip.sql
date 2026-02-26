-- Add pod_ip column to sessions table for agent server communication
ALTER TABLE sessions ADD COLUMN pod_ip TEXT;
