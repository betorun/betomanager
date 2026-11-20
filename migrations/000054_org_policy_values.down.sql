DELETE FROM iam_org_policies WHERE constraint_name <> 'iam.requireMfa';
ALTER TABLE iam_org_policies DROP COLUMN IF EXISTS list_values;
