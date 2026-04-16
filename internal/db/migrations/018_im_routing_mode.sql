ALTER TABLE workspace_im_channels
    ADD COLUMN IF NOT EXISTS routing_mode TEXT NOT NULL DEFAULT 'nanoclaw';
-- Values: 'nanoclaw' (existing flow), 'stateless_cc' (new stateless CC flow)
