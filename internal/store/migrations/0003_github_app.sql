CREATE TABLE IF NOT EXISTS github_installations (
    id TEXT PRIMARY KEY,
    installation_id INTEGER NOT NULL UNIQUE,
    account_login TEXT NOT NULL,
    account_type TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE projects ADD COLUMN github_repo_full_name TEXT;
ALTER TABLE projects ADD COLUMN github_installation_id INTEGER;
ALTER TABLE projects ADD COLUMN github_default_branch TEXT;

CREATE INDEX IF NOT EXISTS idx_github_installations_installation_id ON github_installations(installation_id);
CREATE INDEX IF NOT EXISTS idx_projects_github_installation_id ON projects(github_installation_id);
CREATE INDEX IF NOT EXISTS idx_projects_github_repo_full_name ON projects(github_repo_full_name);
