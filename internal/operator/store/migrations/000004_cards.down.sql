DELETE FROM investigation_steps WHERE kind = 'action';
ALTER TABLE investigation_steps DROP CONSTRAINT investigation_steps_kind_check;
ALTER TABLE investigation_steps ADD CONSTRAINT investigation_steps_kind_check
    CHECK (kind IN ('question', 'answer', 'tool'));
DROP TABLE cards;
