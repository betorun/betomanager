DROP INDEX IF EXISTS idx_support_tickets_assigned;
DELETE FROM support_messages WHERE internal;
ALTER TABLE support_messages DROP COLUMN IF EXISTS internal;
