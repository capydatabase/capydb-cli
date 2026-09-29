-- One verified demo user without a password: sign in through a provider or
-- set a password hash from your auth library.
INSERT INTO users (id, email, email_verified_at, name) VALUES
  ('00000000-0000-4000-8000-000000000001', 'demo@example.com', now(), 'Demo User');
