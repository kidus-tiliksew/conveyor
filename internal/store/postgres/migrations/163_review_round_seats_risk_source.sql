-- req-risk-based-review AC-7.1; req-review-gates-evidence AC-2.7;
-- feature-risk-based-review RR-5. A review round whose seat count an
-- enforcing task's tier set records the source 'risk'. The column's existing
-- CHECK, created inline by migration 149, is replaced with one that also
-- admits 'risk'. Existing rows keep their recorded sources.
ALTER TABLE review_round_seats DROP CONSTRAINT IF EXISTS review_round_seats_seat_count_source_check;
ALTER TABLE review_round_seats ADD CONSTRAINT review_round_seats_seat_count_source_check
 CHECK (seat_count_source IN ('declared', 'planned', 'carried', 'minimum', 'risk'));
