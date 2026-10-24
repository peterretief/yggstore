-- kind: signup
INSERT INTO signups (id, day, name, email)
VALUES (:entry_id, :day, trim(:name), lower(trim(:email)));
