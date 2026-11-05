-- component-identity-membership invariant 35 and component-persistence
-- invariant 41, under DEC-116(3) and req-security-boundaries AC-1.3. Conveyor
-- has no agent credential class, so every stored agent row still live is revoked
-- and refused at its next use. No row is deleted, a row of kind user and
-- an existing revoked_at are never touched, and a rerun changes nothing.
UPDATE user_tokens SET revoked_at = CURRENT_TIMESTAMP(6) WHERE kind='agent' AND revoked_at IS NULL;
