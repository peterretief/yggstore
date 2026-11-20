CREATE TABLE sign_owners (
  -- allow: staff read, staff write
  -- allow: client read own
  name TEXT PRIMARY KEY,
  title TEXT
);

CREATE TABLE sign_owner_people (
  -- allow: staff read, staff write
  -- allow: client read own
  id TEXT PRIMARY KEY,
  owner TEXT NOT NULL REFERENCES sign_owners(name),
  login TEXT NOT NULL
);

CREATE TABLE sign_faults (
  -- menu: Tickets / Faults
  -- mail-in: signs@group.internal, members only, one open per sign
  -- allow: staff read, staff write
  -- allow: client read own, client write own
  -- step: new -> role:staff takes -> open
  -- step: open -> role:staff fixes -> fixed
  -- step: new -> mail recovers -> recovered
  -- step: open -> mail recovers -> recovered
  -- step: recovered -> role:staff closes -> closed
  -- step: fixed -> role:staff closes -> closed
  -- tell: new to role:staff
  id TEXT PRIMARY KEY,
  sign TEXT,                     -- mail: header X-Sign
  owner TEXT REFERENCES sign_owners(name), -- mail: header X-Owner
  site TEXT,                     -- mail: header X-Site
  what TEXT NOT NULL,            -- mail: subject
  details LONGTEXT,              -- mail: text
  assigned TEXT,                 -- read only for: client
  reported_by TEXT,              -- fill: who made it
  status TEXT NOT NULL DEFAULT 'new'
);

CREATE TABLE signs_support (
  -- menu: Tickets / Help
  -- mail-in: signs-support@group.example, acknowledge
  -- allow: staff read, staff write
  -- allow: client read own, client read own through owner, client write own through owner
  -- step: new -> role:staff takes -> open
  -- step: open -> role:staff solves -> solved
  -- tell: new to role:staff
  -- follow-ups: owner, through:owner, role:staff
  id TEXT PRIMARY KEY,
  subject TEXT NOT NULL,         -- mail: subject
  login TEXT,                    -- from: address; fill: who made it
  sign TEXT,                     -- mail: header X-Sign
  owner TEXT REFERENCES sign_owners(name), -- mail: header X-Owner
  body LONGTEXT,                 -- mail: text
  notes LONGTEXT,                -- hide from: client
  status TEXT NOT NULL DEFAULT 'new'
);
