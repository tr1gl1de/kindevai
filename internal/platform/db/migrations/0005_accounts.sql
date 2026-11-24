-- identity context: temporary passwords must be replaced by the user at first sign-in.
ALTER TABLE users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1));
