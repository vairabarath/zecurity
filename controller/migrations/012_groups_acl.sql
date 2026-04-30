-- Sprint 8: Policy engine — groups, group membership, access rules

-- Workspace-scoped groups of users
CREATE TABLE groups (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    description  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, name)
);

CREATE INDEX ON groups(workspace_id);

-- Group membership — join table between groups and users
CREATE TABLE group_members (
    group_id   UUID        NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    joined_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, user_id)
);

-- "Which groups is this user in?" — used by the ACL compiler
CREATE INDEX ON group_members(user_id);

-- Access rules — grant a group access to a resource
CREATE TABLE access_rules (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    resource_id  UUID        NOT NULL REFERENCES resources(id)  ON DELETE CASCADE,
    group_id     UUID        NOT NULL REFERENCES groups(id)     ON DELETE CASCADE,
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (resource_id, group_id)
);

-- Compiler queries: "all rules for a workspace by group" / "by resource"
CREATE INDEX ON access_rules(workspace_id, group_id);
CREATE INDEX ON access_rules(workspace_id, resource_id);
