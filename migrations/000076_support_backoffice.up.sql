-- Back-office support : notes internes de l'équipe (jamais montrées au client).
ALTER TABLE support_messages ADD COLUMN IF NOT EXISTS internal BOOLEAN NOT NULL DEFAULT FALSE;
-- File de l'équipe filtrée par agent.
CREATE INDEX IF NOT EXISTS idx_support_tickets_assigned ON support_tickets (assigned_to_user_id) WHERE status NOT IN ('RESOLVED', 'CLOSED');
