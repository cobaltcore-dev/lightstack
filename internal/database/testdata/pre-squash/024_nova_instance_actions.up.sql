-- Add instance actions tracking table
CREATE TABLE IF NOT EXISTS instance_actions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- No REFERENCES clause, DELIBERATELY (SPEC-025 FR-042). Action records outlive the server they
    -- describe, matching upstream, which retains them for deleted instances. A foreign key with
    -- ON DELETE CASCADE would defeat that; one without a cascade would block the delete entirely.
    -- Growth is bounded by a retention age instead — see migration 068 and nova.instance_action_retention.
    -- Do not "fix" this into a cascade.
    instance_id UUID NOT NULL,
    action VARCHAR(255) NOT NULL,
    request_id VARCHAR(255),
    user_id UUID,
    project_id UUID NOT NULL,
    start_time TIMESTAMP DEFAULT NOW(),
    message TEXT,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_instance_actions_instance ON instance_actions(instance_id);
CREATE INDEX idx_instance_actions_request ON instance_actions(request_id);
