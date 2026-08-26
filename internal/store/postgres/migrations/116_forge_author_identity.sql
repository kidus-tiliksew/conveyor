ALTER TABLE github_lifecycles
    ADD COLUMN forge_author_class text NOT NULL DEFAULT 'workspace',
    ADD COLUMN forge_author_user_id text NOT NULL DEFAULT '',
    ADD CONSTRAINT github_lifecycles_forge_author_class_check
        CHECK (forge_author_class IN ('executing_user', 'approving_operator', 'workspace')),
    ADD CONSTRAINT github_lifecycles_forge_author_user_check
        CHECK ((forge_author_class = 'workspace' AND forge_author_user_id = '') OR
               (forge_author_class IN ('executing_user', 'approving_operator') AND forge_author_user_id <> ''));

ALTER TABLE review_publications
    ADD COLUMN forge_author_class text NOT NULL DEFAULT 'workspace',
    ADD COLUMN forge_author_user_id text NOT NULL DEFAULT '',
    ADD CONSTRAINT review_publications_forge_author_class_check
        CHECK (forge_author_class IN ('executing_user', 'approving_operator', 'workspace')),
    ADD CONSTRAINT review_publications_forge_author_user_check
        CHECK ((forge_author_class = 'workspace' AND forge_author_user_id = '') OR
               (forge_author_class IN ('executing_user', 'approving_operator') AND forge_author_user_id <> ''));
