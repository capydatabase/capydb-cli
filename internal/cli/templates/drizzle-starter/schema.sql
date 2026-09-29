-- drizzle-starter: users and their posts. Run `capydb init drizzle` afterwards
-- to scaffold drizzle.config.ts and a Drizzle schema generated from these tables.
CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email text NOT NULL UNIQUE,
  name text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE posts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  author_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  title text NOT NULL,
  body text NOT NULL DEFAULT '',
  published boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX posts_author_id_idx ON posts (author_id);
