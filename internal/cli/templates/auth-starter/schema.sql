-- auth-starter: the tables a session-based auth setup needs - users, linked
-- OAuth accounts, sessions, and one-time verification tokens. Tokens are
-- stored hashed; store a hash of the value you hand out, never the value.
CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email text NOT NULL,
  email_verified_at timestamptz,
  name text,
  image text,
  password_hash text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- Case-insensitive uniqueness: Ada@example.com and ada@example.com are one user.
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE accounts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  provider text NOT NULL,
  provider_account_id text NOT NULL,
  access_token text,
  refresh_token text,
  expires_at timestamptz,
  scope text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, provider_account_id)
);

CREATE INDEX accounts_user_id_idx ON accounts (user_id);

CREATE TABLE sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  token_hash text NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  ip_address inet,
  user_agent text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE verification_tokens (
  identifier text NOT NULL,
  token_hash text NOT NULL,
  expires_at timestamptz NOT NULL,
  PRIMARY KEY (identifier, token_hash)
);
