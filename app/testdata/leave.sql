CREATE TABLE leave (
  -- allow: staff read own, staff write own
  -- step: draft -> owner submits -> submitted
  -- step: submitted -> role:admin approves -> approved
  -- step: submitted -> role:admin declines -> declined
  -- step: approved -> owner cancels -> cancelled
  -- tell: submitted to role:admin
  -- tell: approved, declined to owner
  -- remind: submitted after 3 days
  id TEXT PRIMARY KEY,
  login TEXT NOT NULL,
  first_day DATE NOT NULL,
  last_day DATE NOT NULL,
  reason TEXT,
  status TEXT NOT NULL DEFAULT 'draft'
);
