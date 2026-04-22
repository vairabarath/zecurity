ALTER TABLE resources DROP CONSTRAINT resources_status_check;
ALTER TABLE resources ADD CONSTRAINT resources_status_check
  CHECK (status IN ('pending','managing','protecting','protected',
                    'unprotected','failed','removing','deleted'));
