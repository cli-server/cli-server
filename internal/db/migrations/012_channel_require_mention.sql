ALTER TABLE workspace_im_channels ADD COLUMN IF NOT EXISTS require_mention BOOLEAN NOT NULL DEFAULT false;
