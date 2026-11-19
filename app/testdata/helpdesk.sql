CREATE TABLE tickets (
  -- mail-in: support@client.example, acknowledge
  -- allow: staff read, staff write
  -- allow: client read own
  -- step: new -> role:staff takes -> open
  -- step: open -> role:staff asks -> waiting
  -- step: waiting -> owner answers -> open
  -- step: open -> role:staff solves -> solved
  -- step: solved -> owner reopens -> open
  -- step: solved -> role:staff closes -> closed
  -- tell: new to role:staff
  -- tell: waiting, solved to owner
  -- remind: new after 1 day
  -- remind: open after 3 days
  id TEXT PRIMARY KEY,
  login TEXT,                    -- from: address (owner)
  sender_name TEXT,              -- from: name
  subject TEXT NOT NULL,         -- mail: subject
  body LONGTEXT,                 -- mail: text
  received DATETIME,             -- mail: received
  member BOOLEAN,                -- mail: in the group
  assigned TEXT,
  priority TEXT CHECK (priority IN ('low', 'normal', 'urgent')),
  status TEXT NOT NULL DEFAULT 'new'
);

CREATE TABLE leave_requests (
  -- mail-in: leave@group.example, members only
  -- allow: staff read own
  id TEXT PRIMARY KEY,
  login TEXT NOT NULL,           -- from: address
  subject TEXT,                  -- mail: subject
  body LONGTEXT                  -- mail: text
);
