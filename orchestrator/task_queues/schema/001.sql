CREATE TABLE IF NOT EXISTS am_schema_version (version INT PRIMARY KEY) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS am_tasks (
 id BIGINT PRIMARY KEY AUTO_INCREMENT, queue_id VARCHAR(128) NOT NULL,
 workflow_id VARCHAR(128) NOT NULL, task_type VARCHAR(128) NOT NULL,
 task_order INT NOT NULL DEFAULT 0, priority INT NOT NULL DEFAULT 0,
 parameters JSON NOT NULL,
 producer_key VARCHAR(255) NULL, request_hash CHAR(64) NULL,
 depends_on BIGINT NULL, status VARCHAR(32) NOT NULL DEFAULT 'queued',
 human_review_required BOOLEAN NOT NULL DEFAULT TRUE,
 attempt_count INT NOT NULL DEFAULT 0, max_attempts INT NOT NULL DEFAULT 3,
 available_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 accepted_result JSON NULL, latest_attempt VARCHAR(36) NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 FOREIGN KEY (depends_on) REFERENCES am_tasks(id),
 UNIQUE KEY producer_task (queue_id,workflow_id,producer_key),
 INDEX eligible (queue_id,status,available_at,priority),
 CHECK (max_attempts > 0 AND attempt_count >= 0),
 CHECK (status IN ('queued','running','awaiting_review','completed','blocked','retry_wait','failed','cancelled'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS am_task_attempts (
 id VARCHAR(36) PRIMARY KEY, task_id BIGINT NOT NULL, queue_id VARCHAR(128) NOT NULL,
 owner VARCHAR(64) NOT NULL, backend_id VARCHAR(64) NOT NULL, status VARCHAR(32) NOT NULL DEFAULT 'claimed',
 lease_expires DATETIME(6) NOT NULL, started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 heartbeat_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), completed_at DATETIME(6) NULL,
 conversation_id VARCHAR(64) NULL, workspace TEXT NULL, input_snapshot JSON NULL,
 resource_usage JSON NULL, submission JSON NULL, submission_hash VARCHAR(64) NULL, error_text TEXT NULL,
 FOREIGN KEY (task_id) REFERENCES am_tasks(id),
 INDEX live (queue_id,status,lease_expires),
 CHECK (status IN ('claimed','running','submitted','completed','failed','blocked','cancelled','awaiting_review'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS am_task_rounds (
 id VARCHAR(32) PRIMARY KEY, attempt_id VARCHAR(36) NOT NULL,
 round_number INT NOT NULL, role VARCHAR(16) NOT NULL,
 prompt MEDIUMTEXT NOT NULL, submission JSON NULL, submission_hash CHAR(64) NULL,
 conversation_id VARCHAR(64) NULL, resource_usage JSON NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), completed_at DATETIME(6) NULL,
 FOREIGN KEY (attempt_id) REFERENCES am_task_attempts(id),
 UNIQUE KEY attempt_round_role (attempt_id,round_number,role),
 CHECK (role IN ('worker','reviewer'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS am_work_log (
 id BIGINT PRIMARY KEY AUTO_INCREMENT, queue_id VARCHAR(128) NOT NULL,
 task_id BIGINT NULL, attempt_id VARCHAR(36) NULL, actor VARCHAR(128) NOT NULL,
 event_type VARCHAR(64) NOT NULL, summary TEXT NOT NULL, data JSON NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 INDEX replay (queue_id,id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

INSERT IGNORE INTO am_schema_version(version) VALUES (1);
