-- Whether the failing alert for the current failure run was sent: a short
-- glitch alerts neither failing nor recovered.
ALTER TABLE destinations ADD COLUMN failing_alerted INTEGER NOT NULL DEFAULT 0;
