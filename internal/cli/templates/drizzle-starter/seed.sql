INSERT INTO users (id, email, name) VALUES
  ('00000000-0000-4000-8000-000000000001', 'ada@example.com', 'Ada'),
  ('00000000-0000-4000-8000-000000000002', 'grace@example.com', 'Grace');

INSERT INTO posts (author_id, title, body, published) VALUES
  ('00000000-0000-4000-8000-000000000001', 'Hello, CapyDB', 'The first post.', true),
  ('00000000-0000-4000-8000-000000000001', 'Drafts stay private', 'Not published yet.', false),
  ('00000000-0000-4000-8000-000000000002', 'Notes on compilers', 'Published by Grace.', true);
