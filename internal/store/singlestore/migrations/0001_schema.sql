-- SingleStore version one, current relational domains at c9a43b66.
-- This is a fresh schema, not a replay of PostgreSQL migrations.
-- Unsupported constraints are listed in README.md.

CREATE ROWSTORE TABLE IF NOT EXISTS `artifact_links` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `artifact_id` LONGTEXT NOT NULL,
 `task_id` LONGTEXT,
 `feature_id` LONGTEXT,
 `role` LONGTEXT NOT NULL DEFAULT 'task_context',
 `requirement_id` LONGTEXT,
 `planning_session_id` LONGTEXT,
 PRIMARY KEY (`workspace_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `artifacts` (
 `id` VARCHAR(255) NOT NULL,
 `workspace_id` VARCHAR(255) NOT NULL,
 `name` LONGTEXT NOT NULL,
 `content_type` LONGTEXT NOT NULL DEFAULT 'application/octet-stream',
 `size_bytes` BIGINT NOT NULL,
 `content` LONGBLOB NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `dashboard_sessions` (
 `id` VARCHAR(255) NOT NULL,
 `user_id` LONGTEXT NOT NULL,
 `session_hash` LONGBLOB NOT NULL,
 `expires_at` DATETIME(6) NOT NULL,
 `last_used_at` DATETIME(6),
 `revoked_at` DATETIME(6),
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `established_by_link` BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY (`id`),
 UNIQUE KEY `dashboard_sessions_session_hash_key` (`id`, `session_hash`),
 SHARD KEY (`id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `decision_sequences` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `high_water_mark` INT NOT NULL DEFAULT 0,
 PRIMARY KEY (`workspace_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `decision_supersession_sweeps` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `decision_id` VARCHAR(255) NOT NULL,
 `superseded_decision_id` LONGTEXT NOT NULL,
 `document_tier` VARCHAR(255) NOT NULL,
 `document_id` VARCHAR(255) NOT NULL,
 `status` LONGTEXT NOT NULL,
 `detected_by` LONGTEXT NOT NULL,
 `detected_at` DATETIME(6) NOT NULL,
 `resolved_by` LONGTEXT NOT NULL DEFAULT '',
 `resolved_at` DATETIME(6),
 PRIMARY KEY (`workspace_id`, `decision_id`, `document_tier`, `document_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `decisions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` VARCHAR(255) NOT NULL,
 `statement` LONGTEXT NOT NULL,
 `context` LONGTEXT NOT NULL,
 `alternatives_rejected` LONGTEXT NOT NULL,
 `status` LONGTEXT NOT NULL,
 `origin` LONGTEXT NOT NULL,
 `origin_session_id` LONGTEXT,
 `origin_task_id` LONGTEXT,
 `supersedes` LONGTEXT,
 `confirmed_by` LONGTEXT,
 `confirmed_at` DATETIME(6),
 `superseded_by` LONGTEXT,
 `created_at` DATETIME(6) NOT NULL,
 `dismissed_by` LONGTEXT,
 `dismissed_at` DATETIME(6),
 PRIMARY KEY (`workspace_id`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `deployment_events` (
 `id` BIGINT NOT NULL AUTO_INCREMENT,
 `org_id` VARCHAR(255) NOT NULL DEFAULT 'deployment',
 `kind` LONGTEXT NOT NULL,
 `actor_id` LONGTEXT NOT NULL,
 `actor_role` LONGTEXT NOT NULL,
 `payload_json` JSON NOT NULL,
 `at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`id`),
 KEY `deployment_events_timeline_idx` (`org_id`, `at`, `id`),
 SHARD KEY (`id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `events` (
 `id` BIGINT NOT NULL AUTO_INCREMENT,
 `task_id` VARCHAR(255),
 `job_id` LONGTEXT,
 `kind` LONGTEXT NOT NULL,
 `actor_id` LONGTEXT NOT NULL,
 `actor_role` LONGTEXT NOT NULL,
 `payload_json` JSON NOT NULL,
 `at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `workspace_id` VARCHAR(255) NOT NULL,
 PRIMARY KEY (`workspace_id`, `id`),
 KEY `events_task_timeline_idx` (`task_id`, `at`, `id`),
 KEY `events_task_id_idx` (`task_id`, `id`),
 KEY `events_workspace_timeline_idx` (`workspace_id`, `at`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `features` (
 `id` VARCHAR(255) NOT NULL,
 `workspace_id` VARCHAR(255) NOT NULL,
 `parent_id` VARCHAR(255),
 `name` VARCHAR(255) NOT NULL,
 `description` LONGTEXT NOT NULL DEFAULT '',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `id`),
 UNIQUE KEY `features_workspace_id_parent_id_name_key` (`workspace_id`, `parent_id`, `name`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `github_lifecycles` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `repository` LONGTEXT NOT NULL,
 `spec_version` INT NOT NULL,
 `source` LONGTEXT NOT NULL DEFAULT '',
 `source_issue_number` INT NOT NULL DEFAULT 0,
 `issue_number` INT NOT NULL DEFAULT 0,
 `issue_url` LONGTEXT NOT NULL DEFAULT '',
 `outcome` LONGTEXT NOT NULL DEFAULT '',
 `state` VARCHAR(255) NOT NULL,
 `attempts` INT NOT NULL DEFAULT 0,
 `last_error` LONGTEXT NOT NULL DEFAULT '',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `create_state` LONGTEXT NOT NULL DEFAULT 'not_started',
 `create_attempts` INT NOT NULL DEFAULT 0,
 `reconcile_misses` INT NOT NULL DEFAULT 0,
 `forge_error_category` LONGTEXT NOT NULL DEFAULT '',
 `forge_author_class` LONGTEXT NOT NULL DEFAULT 'workspace',
 `forge_author_user_id` LONGTEXT NOT NULL DEFAULT '',
 PRIMARY KEY (`workspace_id`, `task_id`),
 KEY `github_lifecycles_state_idx` (`workspace_id`, `state`, `updated_at`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `interrupted_review_recoveries` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `request_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `review_round` INT NOT NULL,
 `actor_id` LONGTEXT NOT NULL,
 `result_json` JSON NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `request_id`),
 KEY `interrupted_review_recoveries_task_round_idx` (`workspace_id`, `task_id`, `review_round`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `interventions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` BIGINT NOT NULL AUTO_INCREMENT,
 `task_id` VARCHAR(255) NOT NULL,
 `job_id` LONGTEXT,
 `actor_id` LONGTEXT NOT NULL,
 `actor_role` LONGTEXT NOT NULL,
 `action` LONGTEXT NOT NULL,
 `reason_code` LONGTEXT NOT NULL,
 `comment` LONGTEXT NOT NULL DEFAULT '',
 `at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `id`),
 KEY `interventions_task_idx` (`task_id`, `at`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `invitation_signin_tokens` (
 `id` VARCHAR(255) NOT NULL,
 `email` LONGTEXT NOT NULL,
 `user_id` LONGTEXT,
 `token_hash` LONGBLOB NOT NULL,
 `expires_at` DATETIME(6) NOT NULL,
 `redeemed_at` DATETIME(6),
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`id`),
 UNIQUE KEY `invitation_signin_tokens_token_hash_key` (`id`, `token_hash`),
 SHARD KEY (`id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `jobs` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `stage` LONGTEXT NOT NULL,
 `harness` LONGTEXT NOT NULL,
 `model_tier` LONGTEXT NOT NULL DEFAULT '',
 `runner` LONGTEXT NOT NULL,
 `pack_version` LONGTEXT NOT NULL DEFAULT '',
 `confinement_tier` LONGTEXT NOT NULL,
 `cost_usd` DOUBLE,
 `tokens_in` BIGINT NOT NULL DEFAULT 0,
 `tokens_out` BIGINT NOT NULL DEFAULT 0,
 `state` LONGTEXT NOT NULL,
 `started_at` DATETIME(6),
 `ended_at` DATETIME(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `auth_mode` LONGTEXT NOT NULL DEFAULT '',
 UNIQUE KEY `jobs_pkey` (`workspace_id`, `id`),
 KEY `jobs_task_started_idx` (`task_id`, `started_at`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `legacy_spec_gate_versions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `spec_version` INT NOT NULL,
 `captured_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `task_id`, `spec_version`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `lineage_repair_exclusions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `src_type` VARCHAR(255) NOT NULL,
 `src_id` VARCHAR(255) NOT NULL,
 `dst_type` VARCHAR(255) NOT NULL,
 `dst_id` VARCHAR(255) NOT NULL,
 `kind` VARCHAR(255) NOT NULL,
 `reason` LONGTEXT NOT NULL,
 `created_by_event_id` BIGINT,
 PRIMARY KEY (`workspace_id`, `src_type`, `src_id`, `dst_type`, `dst_id`, `kind`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `links` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `src_type` VARCHAR(255) NOT NULL,
 `src_id` VARCHAR(255) NOT NULL,
 `dst_type` VARCHAR(255) NOT NULL,
 `dst_id` VARCHAR(255) NOT NULL,
 `kind` VARCHAR(255) NOT NULL,
 `legacy_created_by_event` LONGTEXT,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `created_by_event_id` BIGINT,
 PRIMARY KEY (`workspace_id`, `src_type`, `src_id`, `dst_type`, `dst_id`, `kind`),
 KEY `links_destination_idx` (`workspace_id`, `dst_type`, `dst_id`, `kind`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `monitor_activity` (
 `id` BIGINT NOT NULL AUTO_INCREMENT,
 `workspace_id` VARCHAR(255) NOT NULL,
 `kind` LONGTEXT NOT NULL,
 `payload_json` JSON NOT NULL DEFAULT '{}',
 `at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `monitor_observations` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `identity` VARCHAR(255) NOT NULL,
 `repository` LONGTEXT NOT NULL,
 `kind` LONGTEXT NOT NULL,
 `occurrence_id` LONGTEXT NOT NULL,
 `source_url` LONGTEXT NOT NULL,
 `commit_sha` LONGTEXT NOT NULL DEFAULT '',
 `pull_request_number` INT NOT NULL DEFAULT 0,
 `check_run_id` LONGTEXT NOT NULL DEFAULT '',
 `requirement_id` LONGTEXT,
 `observed_at` DATETIME(6) NOT NULL,
 `context_json` JSON NOT NULL DEFAULT '{}',
 `hint_context_json` JSON,
 `task_id` LONGTEXT,
 `task_outcome` LONGTEXT NOT NULL DEFAULT '',
 `state` LONGTEXT NOT NULL DEFAULT 'observed',
 `deduplicated_count` INT NOT NULL DEFAULT 0,
 `forge_error_category` LONGTEXT NOT NULL DEFAULT '',
 `last_error` LONGTEXT NOT NULL DEFAULT '',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `changed_paths` JSON NOT NULL DEFAULT '[]',
 `causal_event_id` BIGINT,
 PRIMARY KEY (`workspace_id`, `identity`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `monitor_status` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `last_successful_at` DATETIME(6),
 `current_error` LONGTEXT NOT NULL DEFAULT '',
 `forge_error_category` LONGTEXT NOT NULL DEFAULT '',
 `backoff_until` DATETIME(6),
 PRIMARY KEY (`workspace_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `orgs` (
 `id` VARCHAR(255) NOT NULL,
 `name` LONGTEXT NOT NULL,
 `singleton` BOOLEAN NOT NULL DEFAULT true,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`id`),
 UNIQUE KEY `orgs_singleton_key` (`id`, `singleton`),
 SHARD KEY (`id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `planning_bundles` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` VARCHAR(255) NOT NULL,
 `session_id` LONGTEXT NOT NULL,
 `title` LONGTEXT NOT NULL,
 `documents` JSON NOT NULL,
 `tasks` JSON NOT NULL,
 `status` LONGTEXT NOT NULL,
 `created_by` LONGTEXT,
 `decided_by` LONGTEXT,
 `created_at` DATETIME(6) NOT NULL,
 `decided_at` DATETIME(6),
 PRIMARY KEY (`workspace_id`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `planning_messages` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `session_id` VARCHAR(255) NOT NULL,
 `seq` INT NOT NULL,
 `role` LONGTEXT NOT NULL,
 `content` LONGTEXT NOT NULL DEFAULT '',
 `parts_json` JSON NOT NULL DEFAULT '[]',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `session_id`, `seq`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `planning_sessions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` VARCHAR(255) NOT NULL,
 `title` LONGTEXT NOT NULL DEFAULT '',
 `status` LONGTEXT NOT NULL DEFAULT 'active',
 `requirement_context_id` LONGTEXT,
 `produced_requirement_id` LONGTEXT,
 `produced_task_id` LONGTEXT,
 `transcript_artifact_id` LONGTEXT,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `finalized_at` DATETIME(6),
 `model` LONGTEXT NOT NULL DEFAULT '',
 `effort` LONGTEXT NOT NULL DEFAULT '',
 `exploration_output_tokens` INT NOT NULL DEFAULT 0,
 `exploration_tokens_used` BIGINT NOT NULL DEFAULT 0,
 `primary_repo` LONGTEXT NOT NULL DEFAULT '',
 `pinned_revisions` JSON NOT NULL DEFAULT '{}',
 `goal` LONGTEXT NOT NULL DEFAULT 'open',
 `promotion` JSON,
 `system_design_context_id` LONGTEXT,
 `produced_system_design_id` LONGTEXT,
 `produced_bundle_id` LONGTEXT,
 PRIMARY KEY (`workspace_id`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `reference_document_versions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `document_id` VARCHAR(255) NOT NULL,
 `version` INT NOT NULL,
 `filename` LONGTEXT NOT NULL,
 `content_type` LONGTEXT NOT NULL,
 `content` LONGTEXT NOT NULL,
 `supersedes_version` INT,
 `created_by` LONGTEXT NOT NULL,
 `created_at` DATETIME(6) NOT NULL,
 PRIMARY KEY (`workspace_id`, `document_id`, `version`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `reference_documents` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` VARCHAR(255) NOT NULL,
 `name` LONGTEXT NOT NULL,
 `current_version` INT NOT NULL,
 `deleted_at` DATETIME(6),
 `created_at` DATETIME(6) NOT NULL,
 `updated_at` DATETIME(6) NOT NULL,
 PRIMARY KEY (`workspace_id`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `repos` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `name` VARCHAR(255) NOT NULL,
 `url` LONGTEXT NOT NULL,
 `github_slug` LONGTEXT NOT NULL DEFAULT '',
 `default_base` LONGTEXT NOT NULL,
 `devcontainer_path` LONGTEXT NOT NULL DEFAULT '',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `name`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `repository_drift` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` VARCHAR(255) NOT NULL,
 `repository` LONGTEXT NOT NULL,
 `kind` LONGTEXT NOT NULL,
 `source_url` LONGTEXT NOT NULL,
 `commit_sha` LONGTEXT NOT NULL DEFAULT '',
 `requirement_id` LONGTEXT,
 `task_id` LONGTEXT NOT NULL,
 `detected_at` DATETIME(6) NOT NULL,
 `resolved_at` DATETIME(6),
 `outcome` LONGTEXT NOT NULL DEFAULT '',
 `system_design_id` LONGTEXT,
 `system_design_version` INT,
 `causal_event_id` BIGINT,
 `matching_paths` JSON NOT NULL DEFAULT '[]',
 PRIMARY KEY (`workspace_id`, `id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `requirement_versions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `requirement_id` VARCHAR(255) NOT NULL,
 `version` INT NOT NULL,
 `content` LONGTEXT NOT NULL,
 `statements_json` JSON NOT NULL DEFAULT '[]',
 `origin` LONGTEXT NOT NULL,
 `origin_session_id` LONGTEXT NOT NULL DEFAULT '',
 `origin_drift_id` LONGTEXT NOT NULL DEFAULT '',
 `confirmed` BOOLEAN NOT NULL DEFAULT false,
 `confirmed_by` LONGTEXT NOT NULL DEFAULT '',
 `confirmed_at` DATETIME(6),
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `derived_from` JSON,
 `retired` BOOLEAN NOT NULL DEFAULT false,
 `retired_by` LONGTEXT NOT NULL DEFAULT '',
 `retired_at` DATETIME(6),
 `retired_by_version` INT,
 `origin_task_id` LONGTEXT NOT NULL DEFAULT '',
 PRIMARY KEY (`workspace_id`, `requirement_id`, `version`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `requirements` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` VARCHAR(255) NOT NULL,
 `slug` VARCHAR(255) NOT NULL,
 `title` LONGTEXT NOT NULL,
 `current_version` INT,
 `statement_high_water_mark` INT NOT NULL DEFAULT 0,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `archived_at` DATETIME(6),
 `archived_by` LONGTEXT NOT NULL DEFAULT '',
 `superseded_by` JSON NOT NULL DEFAULT '[]',
 PRIMARY KEY (`workspace_id`, `id`),
 UNIQUE KEY `requirements_workspace_id_slug_key` (`workspace_id`, `slug`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `review_publications` (
 `review_work_order_id` VARCHAR(255) NOT NULL,
 `workspace_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `job_id` LONGTEXT NOT NULL,
 `verdict` LONGTEXT NOT NULL,
 `reason_code` LONGTEXT NOT NULL,
 `summary` LONGTEXT NOT NULL,
 `feedback` LONGTEXT NOT NULL DEFAULT '',
 `reviewed_commit_sha` LONGTEXT NOT NULL DEFAULT '',
 `reviewer_model` LONGTEXT NOT NULL DEFAULT '',
 `reviewer_session` LONGTEXT NOT NULL DEFAULT 'distinct',
 `same_model_as_implementer` LONGTEXT NOT NULL DEFAULT 'unknown',
 `state` LONGTEXT NOT NULL,
 `attempts` INT NOT NULL DEFAULT 0,
 `check_run_id` BIGINT NOT NULL DEFAULT 0,
 `comment_id` BIGINT NOT NULL DEFAULT 0,
 `last_error` LONGTEXT NOT NULL DEFAULT '',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `review_round` INT NOT NULL DEFAULT 0,
 `review_seat` INT NOT NULL DEFAULT 0,
 `required_model` LONGTEXT NOT NULL DEFAULT '',
 `required_harness` LONGTEXT NOT NULL DEFAULT '',
 `model_enforcement` LONGTEXT NOT NULL DEFAULT '',
 `required_effort` LONGTEXT NOT NULL DEFAULT '',
 `forge_error_category` LONGTEXT NOT NULL DEFAULT '',
 `forge_author_class` LONGTEXT NOT NULL DEFAULT 'workspace',
 `forge_author_user_id` LONGTEXT NOT NULL DEFAULT '',
 PRIMARY KEY (`workspace_id`, `review_work_order_id`),
 KEY `review_publications_task_idx` (`task_id`, `created_at`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `review_round_retries` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `request_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `reason` LONGTEXT NOT NULL,
 `prior_round` INT NOT NULL,
 `new_round` INT NOT NULL,
 `pr_head` LONGTEXT NOT NULL,
 `actor_id` LONGTEXT NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `request_id`),
 UNIQUE KEY `review_round_retries_task_round_idx` (`workspace_id`, `task_id`, `new_round`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `system_design_versions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `document_id` VARCHAR(255) NOT NULL,
 `version` INT NOT NULL,
 `content` LONGTEXT NOT NULL,
 `governs` JSON NOT NULL,
 `origin` LONGTEXT NOT NULL,
 `origin_session_id` LONGTEXT,
 `origin_task_id` LONGTEXT,
 `confirmed` BOOLEAN NOT NULL DEFAULT false,
 `confirmed_by` LONGTEXT,
 `confirmed_at` DATETIME(6),
 `created_at` DATETIME(6) NOT NULL,
 `dismissed` BOOLEAN NOT NULL DEFAULT false,
 `dismissed_by` LONGTEXT,
 `dismissed_at` DATETIME(6),
 PRIMARY KEY (`workspace_id`, `document_id`, `version`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `system_designs` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `id` VARCHAR(255) NOT NULL,
 `slug` VARCHAR(255) NOT NULL,
 `title` LONGTEXT NOT NULL,
 `category` LONGTEXT NOT NULL,
 `current_version` INT,
 `created_at` DATETIME(6) NOT NULL,
 `updated_at` DATETIME(6) NOT NULL,
 `archived_at` DATETIME(6),
 `archived_by` LONGTEXT NOT NULL DEFAULT '',
 `superseded_by` JSON NOT NULL DEFAULT '[]',
 PRIMARY KEY (`workspace_id`, `id`),
 UNIQUE KEY `system_designs_workspace_id_slug_key` (`workspace_id`, `slug`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `task_context_proposals` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `target_kind` VARCHAR(255) NOT NULL,
 `target_id` VARCHAR(255) NOT NULL,
 `target_title` LONGTEXT NOT NULL,
 `state` VARCHAR(255) NOT NULL,
 `source` LONGTEXT NOT NULL,
 `justification` LONGTEXT NOT NULL,
 `created_by_event_id` BIGINT NOT NULL,
 `decision_event_id` BIGINT,
 `proposed_by` LONGTEXT NOT NULL,
 `decided_by` LONGTEXT NOT NULL DEFAULT '',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `task_id`, `target_kind`, `target_id`),
 KEY `task_context_proposals_pending_idx` (`workspace_id`, `state`, `task_id`, `target_kind`, `target_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `task_dependencies` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `depends_on_task_id` VARCHAR(255) NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `task_id`, `depends_on_task_id`),
 KEY `task_dependencies_dependency_idx` (`workspace_id`, `depends_on_task_id`, `task_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `task_dependency_additions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `request_id` VARCHAR(255) NOT NULL,
 `task_id` LONGTEXT NOT NULL,
 `depends_on_task_id` LONGTEXT NOT NULL,
 `reason` LONGTEXT NOT NULL,
 `actor_id` LONGTEXT NOT NULL,
 `actor_role` LONGTEXT NOT NULL,
 `added` BOOLEAN NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `request_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `task_dependency_removals` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `request_id` VARCHAR(255) NOT NULL,
 `task_id` LONGTEXT NOT NULL,
 `depends_on_task_id` LONGTEXT NOT NULL,
 `reason` LONGTEXT NOT NULL,
 `actor_id` LONGTEXT NOT NULL,
 `actor_role` LONGTEXT NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `request_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `task_setup_changes` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `request_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `request_json` JSON NOT NULL,
 `result_json` JSON NOT NULL,
 `actor_id` LONGTEXT NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `request_id`),
 KEY `task_setup_changes_task_idx` (`workspace_id`, `task_id`, `created_at`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `task_specs` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `version` INT NOT NULL,
 `content` LONGTEXT NOT NULL,
 `acceptance_count` INT NOT NULL,
 `acceptance` JSON NOT NULL DEFAULT '[]',
 `decomposition` JSON NOT NULL DEFAULT '[]',
 `approved` BOOLEAN NOT NULL DEFAULT false,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `approved_at` DATETIME(6),
 `agent` LONGTEXT NOT NULL DEFAULT '',
 `model` LONGTEXT NOT NULL DEFAULT '',
 PRIMARY KEY (`workspace_id`, `task_id`, `version`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `tasks` (
 `id` VARCHAR(255) NOT NULL,
 `workspace_id` VARCHAR(255) NOT NULL,
 `source` VARCHAR(255) NOT NULL,
 `title` LONGTEXT NOT NULL,
 `body` LONGTEXT NOT NULL DEFAULT '',
 `class` LONGTEXT NOT NULL DEFAULT '',
 `escalation_level` LONGTEXT NOT NULL,
 `repo_name` LONGTEXT NOT NULL,
 `base_branch` LONGTEXT NOT NULL,
 `branch` VARCHAR(255) NOT NULL,
 `state` LONGTEXT NOT NULL,
 `parent_task_id` LONGTEXT,
 `created_at` DATETIME(6) NOT NULL,
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `next_stage` LONGTEXT NOT NULL DEFAULT '',
 `recovery_stage` LONGTEXT NOT NULL DEFAULT '',
 `feature_id` LONGTEXT,
 `intake_key` LONGTEXT,
 `mode` LONGTEXT NOT NULL,
 `spec_approval` BOOLEAN NOT NULL,
 `merge_approval` BOOLEAN NOT NULL,
 `policy_version` INT NOT NULL DEFAULT 0,
 `setup_name` LONGTEXT NOT NULL DEFAULT '',
 `setup_contract` JSON NOT NULL DEFAULT '{}',
 `hold` BOOLEAN NOT NULL DEFAULT false,
 `reviewed_head_sha` LONGTEXT NOT NULL DEFAULT '',
 `approved_head_sha` LONGTEXT NOT NULL DEFAULT '',
 `approval_stale` BOOLEAN NOT NULL DEFAULT false,
 `refresh_baseline_sha` LONGTEXT NOT NULL DEFAULT '',
 `refresh_head_sha` LONGTEXT NOT NULL DEFAULT '',
 `refresh_review_scope` LONGTEXT NOT NULL DEFAULT '',
 `origin_spec_version` INT NOT NULL DEFAULT 0,
 `origin_sub_id` LONGTEXT NOT NULL DEFAULT '',
 `assignee_user_id` LONGTEXT,
 PRIMARY KEY (`workspace_id`, `id`),
 UNIQUE KEY `tasks_branch_key` (`workspace_id`, `branch`),
 KEY `tasks_source_idx` (`source`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `transcripts` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `job_id` VARCHAR(255) NOT NULL,
 `uri` LONGTEXT NOT NULL,
 `redaction_stats` JSON NOT NULL DEFAULT '{}',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `job_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `user_forge_tokens` (
 `user_id` VARCHAR(255) NOT NULL,
 `cipher_nonce` LONGBLOB NOT NULL,
 `ciphertext` LONGBLOB NOT NULL,
 `forge_login` LONGTEXT NOT NULL,
 `stored_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`user_id`),
 SHARD KEY (`user_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `user_tokens` (
 `id` VARCHAR(255) NOT NULL,
 `user_id` VARCHAR(255) NOT NULL,
 `label` LONGTEXT NOT NULL DEFAULT '',
 `token_hash` LONGBLOB NOT NULL,
 `last_used_at` DATETIME(6),
 `revoked_at` DATETIME(6),
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `kind` LONGTEXT NOT NULL DEFAULT 'user',
 `scope` LONGTEXT NOT NULL DEFAULT 'user',
 `deployment_credential` BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY (`id`),
 UNIQUE KEY `user_tokens_token_hash_key` (`id`, `token_hash`),
 KEY `user_tokens_user_idx` (`user_id`, `created_at`),
 SHARD KEY (`id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `users` (
 `id` VARCHAR(255) NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `email` VARCHAR(255) NOT NULL,
 `display_name` LONGTEXT NOT NULL DEFAULT '',
 `status` LONGTEXT NOT NULL DEFAULT 'active',
 `password_hash` LONGTEXT,
 PRIMARY KEY (`id`),
 UNIQUE KEY `users_email_unique` (`id`, `email`),
 SHARD KEY (`id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `work_order_activity_snapshots` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `work_order_id` VARCHAR(255) NOT NULL,
 `attempt_id` LONGTEXT NOT NULL,
 `content` LONGTEXT NOT NULL,
 `captured_at` DATETIME(6) NOT NULL,
 PRIMARY KEY (`workspace_id`, `work_order_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `work_order_preemptions` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `request_id` VARCHAR(255) NOT NULL,
 `work_order_id` VARCHAR(255) NOT NULL,
 `request_json` JSON NOT NULL,
 `result_json` JSON NOT NULL,
 `revoked_attempt_id` LONGTEXT NOT NULL,
 `revoked_session_id` VARCHAR(255) NOT NULL,
 `revoked_worker_id` VARCHAR(255) NOT NULL,
 `actor_id` LONGTEXT NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `request_id`),
 KEY `work_order_preemptions_claim_idx` (`workspace_id`, `work_order_id`, `revoked_worker_id`, `revoked_session_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `work_order_recoveries` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `work_order_id` VARCHAR(255) NOT NULL,
 `request_id` VARCHAR(255) NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `work_order_id`, `request_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `work_order_transcript_captures` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `work_order_id` VARCHAR(255) NOT NULL,
 `attempt_id` VARCHAR(255) NOT NULL,
 `content` LONGTEXT NOT NULL,
 `termination_reason` LONGTEXT NOT NULL,
 `truncated` BOOLEAN NOT NULL DEFAULT false,
 `captured_at` DATETIME(6) NOT NULL,
 PRIMARY KEY (`workspace_id`, `work_order_id`, `attempt_id`),
 KEY `work_order_transcript_captures_history_idx` (`workspace_id`, `work_order_id`, `captured_at`, `attempt_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `work_orders` (
 `id` VARCHAR(255) NOT NULL,
 `workspace_id` VARCHAR(255) NOT NULL,
 `task_id` VARCHAR(255) NOT NULL,
 `job_id` LONGTEXT NOT NULL,
 `stage` LONGTEXT NOT NULL,
 `state` VARCHAR(255) NOT NULL,
 `claimant_id` LONGTEXT NOT NULL DEFAULT '',
 `session_id` LONGTEXT NOT NULL DEFAULT '',
 `client_token_hash` LONGTEXT NOT NULL DEFAULT '',
 `agent` LONGTEXT NOT NULL DEFAULT '',
 `model` LONGTEXT NOT NULL DEFAULT '',
 `lease_expires_at` DATETIME(6),
 `progress` LONGTEXT NOT NULL DEFAULT '',
 `cost_usd` DOUBLE NOT NULL DEFAULT 0,
 `tokens_in` BIGINT NOT NULL DEFAULT 0,
 `tokens_out` BIGINT NOT NULL DEFAULT 0,
 `self_reported` BOOLEAN NOT NULL DEFAULT true,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `queue_entered_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `queue_deadline` DATETIME(6) NOT NULL,
 `execution_started_at` DATETIME(6),
 `execution_deadline` DATETIME(6),
 `redispatch_count` INT NOT NULL DEFAULT 0,
 `worker_id` LONGTEXT NOT NULL DEFAULT '',
 `review_round` INT NOT NULL DEFAULT 0,
 `review_seat` INT NOT NULL DEFAULT 0,
 `required_model` LONGTEXT NOT NULL DEFAULT '',
 `required_harness` LONGTEXT NOT NULL DEFAULT '',
 `model_enforcement` LONGTEXT NOT NULL DEFAULT '',
 `required_harness_config` JSON NOT NULL DEFAULT '{}',
 `execution_timeout` LONGTEXT NOT NULL DEFAULT '',
 `last_attempt_outcome` LONGTEXT NOT NULL DEFAULT '',
 `last_failure_message` LONGTEXT NOT NULL DEFAULT '',
 `last_failure_exit_status` INT,
 `last_failure_at` DATETIME(6),
 `automatic_retry_count` INT NOT NULL DEFAULT 0,
 `next_retry_at` DATETIME(6),
 `retry_suppressed` BOOLEAN NOT NULL DEFAULT false,
 `reason_code` LONGTEXT NOT NULL DEFAULT '',
 `review_kind` LONGTEXT NOT NULL DEFAULT '',
 `review_scope` LONGTEXT NOT NULL DEFAULT '',
 `baseline_sha` LONGTEXT NOT NULL DEFAULT '',
 `head_sha` LONGTEXT NOT NULL DEFAULT '',
 `last_failure_detail` LONGTEXT NOT NULL DEFAULT '',
 `retry_suppression_reason` LONGTEXT NOT NULL DEFAULT '',
 `review_superseded` BOOLEAN NOT NULL DEFAULT false,
 `required_effort` LONGTEXT NOT NULL DEFAULT '',
 `rate_limit` JSON,
 `rate_limit_observed_at` DATETIME(6),
 `queue_blocked_at` DATETIME(6),
 `attempt_id` LONGTEXT NOT NULL DEFAULT '',
 `last_attempt_id` LONGTEXT NOT NULL DEFAULT '',
 `last_failure_category` LONGTEXT NOT NULL DEFAULT '',
 `served_requirement_snapshot` JSON,
 `governance_snapshot` JSON,
 `usage_reported` BOOLEAN NOT NULL DEFAULT false,
 `operator_direction` LONGTEXT NOT NULL DEFAULT '',
 `continuation_session_id` LONGTEXT NOT NULL DEFAULT '',
 `continuation_attempt_id` LONGTEXT NOT NULL DEFAULT '',
 `continuation_harness` LONGTEXT NOT NULL DEFAULT '',
 `continuation_launch_environment` LONGTEXT NOT NULL DEFAULT '',
 `checkpoint` JSON NOT NULL DEFAULT '{}',
 PRIMARY KEY (`workspace_id`, `id`),
 KEY `work_orders_task_idx` (`task_id`, `created_at`),
 KEY `work_orders_queue_idx` (`workspace_id`, `state`, `queue_deadline`, `queue_entered_at`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `worker_pairings` (
 `token_hash` VARCHAR(255) NOT NULL,
 `workspace_id` VARCHAR(255) NOT NULL,
 `expires_at` DATETIME(6) NOT NULL,
 `consumed_at` DATETIME(6),
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `owner_user_id` LONGTEXT,
 PRIMARY KEY (`workspace_id`, `token_hash`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `workers` (
 `id` VARCHAR(255) NOT NULL,
 `workspace_id` VARCHAR(255) NOT NULL,
 `name` VARCHAR(255) NOT NULL,
 `credential_hash` VARCHAR(255) NOT NULL,
 `lease_expires_at` DATETIME(6),
 `last_seen_at` DATETIME(6),
 `revoked_at` DATETIME(6),
 `probe_results` JSON NOT NULL DEFAULT '[]',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `owner_user_id` LONGTEXT,
 PRIMARY KEY (`workspace_id`, `id`),
 UNIQUE KEY `workers_credential_hash_key` (`workspace_id`, `credential_hash`),
 UNIQUE KEY `workers_workspace_id_name_key` (`workspace_id`, `name`),
 KEY `workers_workspace_idx` (`workspace_id`, `created_at`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `workspace_forge_tokens` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `cipher_nonce` LONGBLOB NOT NULL,
 `ciphertext` LONGBLOB NOT NULL,
 `forge_login` LONGTEXT NOT NULL,
 `stored_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `workspace_membership_invitations` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `email` VARCHAR(255) NOT NULL,
 `role` LONGTEXT NOT NULL,
 `invited_by` LONGTEXT NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `email`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `workspace_role_bindings` (
 `workspace_id` VARCHAR(255) NOT NULL,
 `user_id` VARCHAR(255) NOT NULL,
 `role` VARCHAR(255) NOT NULL,
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (`workspace_id`, `user_id`),
 KEY `workspace_role_bindings_user_idx` (`user_id`, `workspace_id`),
 KEY `workspace_role_bindings_user_role_idx` (`user_id`, `role`),
 SHARD KEY (`workspace_id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE ROWSTORE TABLE IF NOT EXISTS `workspaces` (
 `id` VARCHAR(255) NOT NULL,
 `name` VARCHAR(255) NOT NULL,
 `config_yaml` LONGTEXT NOT NULL DEFAULT '',
 `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 `config_version` BIGINT NOT NULL DEFAULT 1,
 `org_id` LONGTEXT NOT NULL DEFAULT 'deployment',
 PRIMARY KEY (`id`),
 UNIQUE KEY `workspaces_name_key` (`id`, `name`),
 SHARD KEY (`id`)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
