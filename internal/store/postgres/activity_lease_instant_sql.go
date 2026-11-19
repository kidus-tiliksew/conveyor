package postgres

import "fmt"

// The PostgreSQL backend owns its lease-instant SQL (component-persistence
// invariant 2: no SQL layer is shared). These are the PostgreSQL spellings of
// the three operations the lease arithmetic needs.
const (
	// markerRegexOp is the case-matching-by-collation regular expression
	// operator.
	markerRegexOp = "~"
	// markerLeaseIntType is the CAST target that holds a 64-bit integer.
	markerLeaseIntType = "bigint"
	// markerLeaseDivOp truncates integer division. Every operand
	// markerLeaseInstantSQL divides is non-negative, so truncation equals
	// floor.
	markerLeaseDivOp = " / "
)

// leaseRFC3339Shape includes the fallback spellings time.Time.UnmarshalJSON
// accepts: a one- or two-digit hour and either fraction separator. Calendar
// and field ranges are checked separately, including Go's inclusive zone
// limits of 24 hours and 60 minutes.
// The pattern holds no `?`, so the text holds no placeholder character.
// Collations may fold case, so SQL also checks T and Z bytes.
const leaseRFC3339Shape = `^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T([0-9]|[0-9][0-9]):[0-9][0-9]:[0-9][0-9]` +
	`(Z|[+-][0-9][0-9]:[0-9][0-9]|[.,][0-9]+(Z|[+-][0-9][0-9]:[0-9][0-9]))$`

// leaseZeroUnix is the Unix second of time.Time{}, the instant Go's
// IsZero reports for a decoded lease.
const leaseZeroUnix = -62135596800

// markerLeaseInstantSQL decodes a JSON-string lease timestamp inside the database
// with the arithmetic of Go's JSON time decoder, never with a vendor
// date-conversion function.
//
// inner is a SELECT that exposes the carry columns plus lease_text, the
// string value of the field (NULL when absent), and lease_string, 1 only when
// the JSON value's type is string (a number, bool, object, null, or missing
// key is 0, where json.Unmarshal into time.Time fails or leaves the zero
// time). carry is the comma-separated list of inner columns to pass through.
//
// The result exposes carry plus lease_ok (1 when Go decodes a non-zero
// time), lease_sec (Unix seconds of the instant), and lease_nanos (its
// nanosecond remainder; digits past the ninth are truncated as Go truncates
// them). Rows with lease_ok=0 carry meaningless instants and must be
// filtered on lease_ok. A decoded zero time reports lease_ok=0 because the
// folds treat the zero time as an absent lease.
//
// Go's fallback parse also accepts a single-digit hour, comma fractions,
// and zone hour <=24 and minute <=60. Dates remain calendar-valid, with
// hour <=23, minute <=59, second <=59, and year 0000-9999. Offsets shift
// the instant, so different zone spellings can represent equal instants.
//
// This timestamp parser does not reproduce Go's field-name matching or
// whole-payload decoding. Callers cannot assume that extracting this field
// alone has the same semantics as unmarshalling a WorkOrder.
func markerLeaseInstantSQL(inner, carry string) string {
	i := markerLeaseIntType
	div := func(a, b string) string { return "(" + a + markerLeaseDivOp + b + ")" }
	num := func(pos, length string) string {
		return fmt.Sprintf("CAST(SUBSTRING(t,%s,%s) AS %s)", pos, length, i)
	}
	// Stage 1: shape. t is the lease text, last is its final byte.
	shape := fmt.Sprintf(`SELECT %[1]s, t,
		CASE WHEN is_str=1 AND t IS NOT NULL AND t %[2]s '%[3]s'
			AND ASCII(SUBSTRING(t,11,1))=84
			AND (ASCII(SUBSTRING(t,CHAR_LENGTH(t),1))=90 OR
				(ASCII(SUBSTRING(t,CHAR_LENGTH(t),1)) BETWEEN 48 AND 57 AND SUBSTRING(t,CHAR_LENGTH(t)-5,1) IN ('+','-')))
			THEN 1 ELSE 0 END AS shaped,
		CASE WHEN SUBSTRING(t,13,1)=':' THEN 1 ELSE 2 END AS hlen,
		CASE WHEN ASCII(SUBSTRING(t,CHAR_LENGTH(t),1))=90 THEN 1 ELSE 6 END AS tzlen
		FROM (SELECT %[1]s, lease_text AS t, lease_string AS is_str FROM (%[4]s) src) named`, carry, markerRegexOp, leaseRFC3339Shape, inner)
	// Stage 2: fields. nlen is the fraction field's length including its dot.
	fields := fmt.Sprintf(`SELECT %[1]s, shaped, tzlen,
		CASE WHEN shaped=1 THEN %[2]s ELSE 0 END AS y,
		CASE WHEN shaped=1 THEN %[3]s ELSE 0 END AS mo,
		CASE WHEN shaped=1 THEN %[4]s ELSE 0 END AS d,
		CASE WHEN shaped=1 THEN %[5]s ELSE 0 END AS h,
		CASE WHEN shaped=1 THEN %[6]s ELSE 0 END AS mi,
		CASE WHEN shaped=1 THEN %[7]s ELSE 0 END AS s,
		CASE WHEN shaped=1 AND CHAR_LENGTH(t)-17-hlen-tzlen>1 THEN CAST(RPAD(SUBSTRING(t,19+hlen,LEAST(9,CHAR_LENGTH(t)-18-hlen-tzlen)),9,'0') AS %[8]s) ELSE 0 END AS nanos,
		CASE WHEN shaped=1 AND tzlen=6 AND SUBSTRING(t,CHAR_LENGTH(t)-5,1)='-' THEN -1 ELSE 1 END AS tzsign,
		CASE WHEN shaped=1 AND tzlen=6 THEN CAST(SUBSTRING(t,CHAR_LENGTH(t)-4,2) AS %[8]s) ELSE 0 END AS tzh,
		CASE WHEN shaped=1 AND tzlen=6 THEN CAST(SUBSTRING(t,CHAR_LENGTH(t)-1,2) AS %[8]s) ELSE 0 END AS tzm
		FROM (%[9]s) shaped_rows`, carry,
		num("1", "4"), num("6", "2"), num("9", "2"), num("12", "hlen"), num("13+hlen", "2"), num("16+hlen", "2"), i, shape)
	// Stage 3: calendar validity and the shifted civil year. Years shift by
	// 400 so every divided operand stays non-negative.
	civil := fmt.Sprintf(`SELECT %[1]s, nanos, tzsign, tzh, tzm, y, mo, d, h, mi, s,
		CASE WHEN shaped=1 AND mo BETWEEN 1 AND 12 AND d>=1
			AND d<=CASE WHEN mo=2 THEN CASE WHEN MOD(y,4)=0 AND (MOD(y,100)<>0 OR MOD(y,400)=0) THEN 29 ELSE 28 END
				WHEN mo IN (4,6,9,11) THEN 30 ELSE 31 END
			AND h<=23 AND mi<=59 AND s<=59 AND tzh<=24 AND tzm<=60 THEN 1 ELSE 0 END AS valid,
		CASE WHEN mo<=2 THEN y+399 ELSE y+400 END AS yy,
		CASE WHEN mo>2 THEN mo-3 ELSE mo+9 END AS mp
		FROM (%[2]s) field_rows`, carry, fields)
	era := div("yy", "400")
	yoe := "(yy-" + era + "*400)"
	days := fmt.Sprintf("(%s*146097+%s*365+%s-%s+%s+d-1-719468-146097)",
		era, yoe, div(yoe, "4"), div(yoe, "100"), div("(153*mp+2)", "5"))
	return fmt.Sprintf(`SELECT %[1]s,
		CASE WHEN valid=1 AND NOT (sec=%[3]d AND nanos=0) THEN 1 ELSE 0 END AS lease_ok,
		sec AS lease_sec, nanos AS lease_nanos
		FROM (SELECT %[1]s, valid, nanos,
			%[2]s*86400+h*3600+mi*60+s-tzsign*(tzh*3600+tzm*60) AS sec
			FROM (%[4]s) civil_rows) epoch_rows`, carry, days, leaseZeroUnix, civil)
}
