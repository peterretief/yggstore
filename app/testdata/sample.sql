-- The design: tables, how they relate, and who may see them. Edit it (or
-- export it from drawDB), then import it; ./dev does that for you.
--
-- Each table's foreign keys are its relationships, and its columns are the
-- fields of its form. A column type of LONGTEXT is a big text box, BOOLEAN a
-- tick box, DATE a date. Importing again after an edit keeps the rows there.
--
-- Who may do what goes inside each table, as comments (comments above a
-- table are lost when it's stored):
--
--   -- allow: staff read, staff write, staff delete
--   -- allow: client read own
--
-- An admin may do everything. Anyone else may do only what a table allows: a
-- table without an allow line is for admins only. "own" is a client's own
-- rows: the row whose login is their email, and whatever links to it through
-- foreign keys. Clients only read, for now.
--
-- After a column, "-- hide from: client" leaves it out for that role (or
-- "staff", or both), "-- label: ..." names it, and "-- help: ..." adds a line
-- under its field. Several go together with ";".
--
-- id TEXT PRIMARY KEY gives each new row a random id, so machines working
-- apart can't both make the same one. What refers to it is TEXT too.

CREATE TABLE clients (
  -- allow: staff read, staff write
  -- allow: client read own
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  email TEXT,
  login TEXT UNIQUE,     -- label: Signs in as; help: The email of their account on People
  notes LONGTEXT         -- hide from: client
);

CREATE TABLE projects (
  -- allow: staff read, staff write
  -- allow: client read own
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  client_id TEXT NOT NULL REFERENCES clients(id),
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'on hold', 'done')),
  started DATE
);

CREATE TABLE invoices (
  -- allow: staff read
  -- allow: client read own
  id TEXT PRIMARY KEY,
  number TEXT NOT NULL UNIQUE,
  project_id TEXT NOT NULL REFERENCES projects(id),
  amount REAL NOT NULL,
  sent DATE,
  paid BOOLEAN NOT NULL DEFAULT 0
);
