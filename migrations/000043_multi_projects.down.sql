ALTER TABLE organizations DROP COLUMN IF EXISTS max_projects;
DROP INDEX IF EXISTS uq_projects_org_name;
ALTER TABLE projects ADD CONSTRAINT projects_name_key UNIQUE (name);
DROP INDEX IF EXISTS uq_projects_namespace;
ALTER TABLE projects DROP COLUMN IF EXISTS namespace;
