CREATE TABLE IF NOT EXISTS project_mounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT NOT NULL,
    mount_type TEXT NOT NULL DEFAULT 'bind' CHECK (mount_type IN ('bind')),
    source TEXT NOT NULL,
    target TEXT NOT NULL,
    read_only INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0, 1)),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (project_id, mount_type, source, target),
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS project_http_routes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT NOT NULL,
    hostname TEXT,
    match_type TEXT NOT NULL CHECK (match_type IN ('host', 'path_prefix', 'path_exact')),
    match_value TEXT,
    strip_prefix INTEGER NOT NULL DEFAULT 0 CHECK (strip_prefix IN (0, 1)),
    rewrite_prefix TEXT,
    priority INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (project_id, hostname, match_type, match_value, priority),
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_project_mounts_project_id ON project_mounts(project_id);
CREATE INDEX IF NOT EXISTS idx_project_http_routes_project_id ON project_http_routes(project_id);
CREATE INDEX IF NOT EXISTS idx_project_http_routes_hostname ON project_http_routes(hostname);
