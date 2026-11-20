-- IAM phase C : politiques d'organisation étendues (iam.md §13). Valeurs de
-- liste d'une contrainte (ex. iam.allowedMemberDomains : domaines autorisés).
ALTER TABLE iam_org_policies ADD COLUMN IF NOT EXISTS list_values JSONB NOT NULL DEFAULT '[]'::jsonb;
