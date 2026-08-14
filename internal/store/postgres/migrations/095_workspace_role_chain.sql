ALTER TABLE workspace_role_bindings
    DROP CONSTRAINT workspace_role_bindings_role_check;
ALTER TABLE workspace_membership_invitations
    DROP CONSTRAINT workspace_membership_invitations_role_check;

UPDATE workspace_role_bindings SET role = 'contributor' WHERE role = 'user';
UPDATE workspace_membership_invitations SET role = 'contributor' WHERE role = 'user';

ALTER TABLE workspace_role_bindings
    ADD CONSTRAINT workspace_role_bindings_role_check
    CHECK (role IN ('viewer', 'executor', 'contributor', 'maintainer', 'operator'));
ALTER TABLE workspace_membership_invitations
    ADD CONSTRAINT workspace_membership_invitations_role_check
    CHECK (role IN ('viewer', 'executor', 'contributor', 'maintainer', 'operator'));
