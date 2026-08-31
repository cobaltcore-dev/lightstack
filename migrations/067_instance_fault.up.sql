-- SPEC-025 (cloud-storage#547) B13 — make a failed boot readable from the server itself.
--
-- A server whose network could not be attached reached ACTIVE with an empty address map and nothing
-- anywhere to say why: the port-allocation failure was logged and the boot continued. A user was told
-- the server was running, it had no connectivity, and `openstack server show` offered no explanation.
--
-- Upstream compute surfaces this as a `fault` object on server details, present only while the server
-- is in ERROR. Reusing instance_actions.message was considered and rejected: a user diagnosing a
-- failed boot runs `server show`, not `server event list`, and Article I fixes the response shape.
--
-- All three nullable with no default: a server that never failed carries three NULLs and reports no
-- fault object at all, which is upstream's behaviour and makes this change invisible to every existing
-- response. `details` is deliberately not modelled — upstream fills it with a traceback for
-- administrators, and Lightstack has none to offer.
ALTER TABLE instances ADD COLUMN IF NOT EXISTS fault_code    INTEGER;
ALTER TABLE instances ADD COLUMN IF NOT EXISTS fault_message TEXT;
ALTER TABLE instances ADD COLUMN IF NOT EXISTS fault_created TIMESTAMP;
