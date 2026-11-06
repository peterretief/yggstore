-- An example design. Edit it, or write your own, then import it:
--
--   ./signin -data clients.db -import design.sql
--
-- Each table's foreign keys are its relationships, and its columns are the
-- fields of its form. A column type of LONGTEXT is a big text box, BOOLEAN a
-- tick box, DATE a date. Importing again after an edit keeps the rows there.

CREATE TABLE clients (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  email TEXT,
  notes LONGTEXT
);

CREATE TABLE projects (
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL,
  client_id INTEGER NOT NULL REFERENCES clients(id),
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'on hold', 'done')),
  started DATE
);

CREATE TABLE invoices (
  id INTEGER PRIMARY KEY,
  number TEXT NOT NULL UNIQUE,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  amount REAL NOT NULL,
  sent DATE,
  paid BOOLEAN NOT NULL DEFAULT 0
);
