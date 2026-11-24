-- network context (KIN-30): network profiles (proxy without its password — that lives in integration_secrets as
-- "network_profile.<id>.password"), their assignment along System → Runner → Project → CLI role, the revision of
-- the configuration delivered to runners and Test Network runs. Times are RFC 3339 UTC text.
CREATE TABLE network_profiles (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE CHECK (name <> ''),
  proxy_scheme   TEXT NOT NULL DEFAULT '' CHECK (proxy_scheme IN ('', 'http', 'https', 'socks5')),
  proxy_host     TEXT NOT NULL DEFAULT '',
  proxy_port     INTEGER NOT NULL DEFAULT 0 CHECK (proxy_port BETWEEN 0 AND 65535),
  proxy_username TEXT NOT NULL DEFAULT '',
  enforcement    TEXT NOT NULL CHECK (enforcement IN ('off', 'monitor', 'strict')),
  dns_policy     TEXT NOT NULL CHECK (dns_policy IN ('system', 'proxy')),
  fail_closed    INTEGER NOT NULL CHECK (fail_closed IN (0, 1)),
  expected_ipv4  TEXT NOT NULL DEFAULT '',
  expected_ipv6  TEXT NOT NULL DEFAULT '',
  allow_json     TEXT NOT NULL DEFAULT '[]',
  diagnostic_url TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  CHECK (enforcement <> 'strict' OR (proxy_scheme <> '' AND fail_closed = 1 AND dns_policy = 'proxy'))
) STRICT;

-- level 'system' has target ''; runner/project targets are ids of those contexts (checked by the use case, no FK
-- across contexts); 'role' targets are CLI names.
CREATE TABLE network_assignments (
  level      TEXT NOT NULL CHECK (level IN ('system', 'runner', 'project', 'role')),
  target     TEXT NOT NULL,
  profile_id TEXT NOT NULL REFERENCES network_profiles(id) ON DELETE RESTRICT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (level, target),
  CHECK ((level = 'system') = (target = ''))
) STRICT;

CREATE TABLE network_state (
  id       INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL CHECK (revision >= 0)
) STRICT;
INSERT INTO network_state (id, revision) VALUES (1, 0);

CREATE TABLE network_tests (
  id            TEXT PRIMARY KEY,
  runner_id     TEXT NOT NULL,
  profile_id    TEXT NOT NULL DEFAULT '',
  requested_by  TEXT NOT NULL DEFAULT '',
  requested_at  TEXT NOT NULL,
  expected_ipv4 TEXT NOT NULL DEFAULT '',
  expected_ipv6 TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL CHECK (status IN ('pending', 'ok', 'failed')),
  code          TEXT NOT NULL DEFAULT '',
  detail        TEXT NOT NULL DEFAULT '',
  observed_ipv4 TEXT NOT NULL DEFAULT '',
  observed_ipv6 TEXT NOT NULL DEFAULT '',
  checks_json   TEXT NOT NULL DEFAULT '[]',
  duration_ms   INTEGER NOT NULL DEFAULT 0,
  completed_at  TEXT
) STRICT;
CREATE INDEX network_tests_runner ON network_tests(runner_id, requested_at DESC);
CREATE INDEX network_tests_requested ON network_tests(requested_at DESC);
