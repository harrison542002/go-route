-- A dashboard is about to read this API, and the people who will open it
-- are not all the people who may change what it shows: finance wants to
-- see what a tenant spent, support wants to see which key is failing, and
-- neither should be one mistyped request away from lifting a quota. Until
-- now every credential could do everything, which made the only safe
-- answer to "can they see spend" be "no".
CREATE TYPE admin_role AS ENUM ('admin', 'readonly');

COMMENT ON TYPE admin_role IS
    'What a credential may do. admin may read and write: tenants, keys and quotas, the whole API. readonly may only read -- the safe methods, GET and HEAD -- so a credential handed to a dashboard viewer can show usage and audit history without being able to change a quota, mint a key or disable a tenant. The split exists because observability and control arrive together over one API, and the second is not implied by wanting the first.';

ALTER TABLE admin_credentials
    ADD COLUMN role       admin_role NOT NULL DEFAULT 'admin',
    ADD COLUMN expires_at TIMESTAMPTZ;

-- The default is here only to give the rows that already exist a role,
-- and they take admin because that is exactly what they could do before
-- this migration: a credential must not quietly lose power because the
-- schema learned a new word. It is dropped immediately afterwards, so
-- from here on every insert names the role it wants and nothing acquires
-- the powerful one by omission.
ALTER TABLE admin_credentials ALTER COLUMN role DROP DEFAULT;

COMMENT ON COLUMN admin_credentials.role IS
    'What this credential may do, checked on every request after the token resolves. A readonly credential attempting a write is refused with 403, not 401: it authenticated, and telling an authenticated caller which role it holds leaks nothing it could not learn by trying.';
COMMENT ON COLUMN admin_credentials.expires_at IS
    'When this credential stops working, or NULL for one that does not expire. Checked at authentication rather than swept by a job: a sweep would have to delete or blank the row, and audit_log rows name the credential that wrote them, so the history would stop being readable. An expired credential therefore stays exactly as attributable as a revoked one -- and is refused with the same 401 as an unknown or revoked one, so a caller probing with a guessed token cannot learn that it once worked.';
