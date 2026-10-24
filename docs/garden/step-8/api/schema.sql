CREATE TABLE workdays (id TEXT PRIMARY KEY, places INTEGER NOT NULL);
INSERT INTO workdays VALUES ('2026-11-07', 10), ('2026-11-21', 10);

CREATE TABLE signups (
  id    TEXT PRIMARY KEY,
  day   TEXT NOT NULL REFERENCES workdays,
  name  TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  email TEXT NOT NULL CHECK (email LIKE '%_@_%'),
  UNIQUE (day, email)
);

CREATE TRIGGER day_full BEFORE INSERT ON signups
WHEN (SELECT count(*) FROM signups WHERE day = NEW.day) >= (SELECT places FROM workdays WHERE id = NEW.day)
BEGIN SELECT RAISE(ABORT, 'the day is full'); END;
