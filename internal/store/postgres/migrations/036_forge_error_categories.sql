ALTER TABLE github_lifecycles
    ADD COLUMN forge_error_category text NOT NULL DEFAULT '',
    ADD CONSTRAINT github_lifecycles_forge_error_category_check
        CHECK (forge_error_category IN (
            '',
            'forge_request',
            'forge_status',
            'forge_response',
            'forge_rate_limited',
            'forge_permission'
        ));

ALTER TABLE review_publications
    ADD COLUMN forge_error_category text NOT NULL DEFAULT '',
    ADD CONSTRAINT review_publications_forge_error_category_check
        CHECK (forge_error_category IN (
            '',
            'forge_request',
            'forge_status',
            'forge_response',
            'forge_rate_limited',
            'forge_permission'
        ));
