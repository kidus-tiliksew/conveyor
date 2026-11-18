package store

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// referenceLeaseInstant is a line-for-line Go port of the arithmetic
// LeaseSQLDialect.LeaseInstantSQL emits: the same shape gate, field slices,
// calendar checks, shifted-year day count, zone shift, and nine-digit
// fraction truncation. It is the executable specification the SQL text is
// written to: the test below proves the arithmetic equals time.Time's
// JSON decoder, so a database that evaluates the SQL as written
// decodes the same instant. It does not run the SQL; the database
// differential conformance does.
func referenceLeaseInstant(text string, isString bool) (sec, nanos int64, ok bool) {
	shape := regexp.MustCompile(leaseRFC3339Shape)
	last := byte(0)
	if len(text) > 0 {
		last = text[len(text)-1]
	}
	shaped := isString && shape.MatchString(text) && len(text) > 10 && text[10] == 'T' &&
		(last == 'Z' || (last >= '0' && last <= '9' && len(text) >= 6 && (text[len(text)-6] == '+' || text[len(text)-6] == '-')))
	if !shaped {
		return 0, 0, false
	}
	tzlen := 6
	if last == 'Z' {
		tzlen = 1
	}
	num := func(pos, length int) int64 {
		value, err := strconv.ParseInt(text[pos-1:pos-1+length], 10, 64)
		if err != nil {
			panic(err)
		}
		return value
	}
	hlen := 2
	if text[12] == ':' {
		hlen = 1
	}
	y, mo, d, h, mi, s := num(1, 4), num(6, 2), num(9, 2), num(12, hlen), num(13+hlen, 2), num(16+hlen, 2)
	if nlen := len(text) - 17 - hlen - tzlen; nlen > 1 {
		start := 18 + hlen
		digits := text[start:min(start+9, start+nlen-1)]
		digits += strings.Repeat("0", 9-len(digits))
		parsed, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			panic(err)
		}
		nanos = parsed
	}
	tzsign, tzh, tzm := int64(1), int64(0), int64(0)
	if tzlen == 6 {
		if text[len(text)-6] == '-' {
			tzsign = -1
		}
		tzh, tzm = num(len(text)-4, 2), num(len(text)-1, 2)
	}
	dim := int64(31)
	switch {
	case mo == 2:
		dim = 28
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			dim = 29
		}
	case mo == 4 || mo == 6 || mo == 9 || mo == 11:
		dim = 30
	}
	if mo < 1 || mo > 12 || d < 1 || d > dim || h > 23 || mi > 59 || s > 59 || tzh > 24 || tzm > 60 {
		return 0, 0, false
	}
	yy := y + 400
	if mo <= 2 {
		yy = y + 399
	}
	mp := mo + 9
	if mo > 2 {
		mp = mo - 3
	}
	era := yy / 400
	yoe := yy - era*400
	days := era*146097 + yoe*365 + yoe/4 - yoe/100 + (153*mp+2)/5 + d - 1 - 719468 - 146097
	sec = days*86400 + h*3600 + mi*60 + s - tzsign*(tzh*3600+tzm*60)
	if sec == leaseZeroUnix && nanos == 0 {
		return 0, 0, false
	}
	return sec, nanos, true
}

// goLeaseInstant is the behavior being matched: json.Unmarshal of the string
// into the time.Time the review lease folds decode, with the zero time
// counted as an absent lease.
func goLeaseInstant(text string) (sec, nanos int64, ok bool) {
	encoded, err := json.Marshal(text)
	if err != nil {
		return 0, 0, false
	}
	var decoded struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	if json.Unmarshal([]byte(`{"lease_expires_at":`+string(encoded)+`}`), &decoded) != nil || decoded.LeaseExpiresAt.IsZero() {
		return 0, 0, false
	}
	return decoded.LeaseExpiresAt.Unix(), int64(decoded.LeaseExpiresAt.Nanosecond()), true
}

func TestLeaseInstantArithmeticMatchesGoDecode(t *testing.T) {
	fixed := []string{
		"2026-11-17T19:47:03Z", "2026-11-17T19:47:03.152142Z", "2026-11-17T19:47:03.152142+05:30", "2026-11-17T19:47:03.152142-08:00",
		"2026-11-17T19:47:03.123456789Z", "2026-11-17T19:47:03.1234567891234Z", "2026-11-17T19:47:03.5Z", "2026-11-17T19:47:03.000000000000Z",
		"2026-11-17T19:47:03+14:00", "2026-11-17T19:47:03-00:00", "2026-11-17T19:47:03+23:59", "2026-11-17T19:47:03+24:00", "2026-11-17T19:47:03+05:60",
		"4838-09-06T04:38:55.25127-17:60", "2026-11-17T9:47:03,1234567891234+24:60",
		"2026-11-17T9:47:03.000000001-24:60", "2026-11-17T0:00:00,5Z",
		"2026-11-17T19:47:03+25:00", "2026-11-17T19:47:03+05:61", "2026-11-17T9:47:03,Z",
		"2026-02-29T9:47:03,5+24:60", "2024-02-29T9:47:03,5+24:60",
		"2026-11-17T19:47:03+0530", "2026-11-17T19:47:03", "2026-11-17T19:47:03ZZ", "2026-11-17T19:47:03Z ", " 2026-11-17T19:47:03Z", "2026-11-17T19:47:03Z\n",
		"2026-11-17t19:47:03Z", "2026-11-17T19:47:03z", "2026-11-17 19:47:03Z", "2026-11-17T19:47:03,5Z", "2026-11-17T19:47:03.Z",
		"2026-11-17T24:00:00Z", "2026-11-17T23:60:00Z", "2026-11-17T23:59:60Z", "2026-11-17T23:59:59Z",
		"2026-13-01T00:00:00Z", "2026-00-10T00:00:00Z", "2026-01-00T00:00:00Z", "2026-02-29T00:00:00Z", "2024-02-29T00:00:00Z",
		"2100-02-29T00:00:00Z", "2400-02-29T00:00:00Z", "2026-04-31T00:00:00Z", "2026-04-30T00:00:00Z", "2026-12-31T23:59:59Z",
		"0001-01-01T00:00:00Z", "0001-01-01T05:30:00+05:30", "0001-01-01T00:00:00.000000001Z", "0000-01-01T00:00:00Z", "0000-02-29T00:00:00Z",
		"9999-12-31T23:59:59.999999999Z", "9999-12-31T23:59:59.999999999-23:59", "1970-01-01T00:00:00Z", "1969-12-31T23:59:59Z",
		"12026-11-17T19:47:03Z", "26-11-17T19:47:03Z", "2026-1-17T19:47:03Z", "2026-11-17T9:47:03Z", "", "not a time", "2026-11-17",
		"２０２６-11-17T19:47:03Z",
	}
	for _, text := range fixed {
		wantSec, wantNanos, wantOK := goLeaseInstant(text)
		gotSec, gotNanos, gotOK := referenceLeaseInstant(text, true)
		if gotOK != wantOK || (wantOK && (gotSec != wantSec || gotNanos != wantNanos)) {
			t.Errorf("%q: arithmetic = (%d,%d,%t), Go decode = (%d,%d,%t)", text, gotSec, gotNanos, gotOK, wantSec, wantNanos, wantOK)
		}
	}
	if _, _, ok := referenceLeaseInstant("2026-11-17T19:47:03Z", false); ok {
		t.Fatal("a lease whose JSON type is not string decoded")
	}

	rng := rand.New(rand.NewPCG(20261117, 1))
	alphabet := []byte("0123456789-:.TZz+, tT/xX")
	for range 20000 {
		year, month, day := rng.IntN(10000), 1+rng.IntN(12), 1+rng.IntN(31)
		text := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", year, month, day, rng.IntN(25), rng.IntN(61), rng.IntN(61))
		if digits := rng.IntN(13); digits > 0 {
			var fraction strings.Builder
			for range digits {
				fraction.WriteByte(byte('0' + rng.IntN(10)))
			}
			text += "." + fraction.String()
		}
		switch rng.IntN(4) {
		case 0:
			text += "Z"
		default:
			sign := "+"
			if rng.IntN(2) == 0 {
				sign = "-"
			}
			text += fmt.Sprintf("%s%02d:%02d", sign, rng.IntN(26), rng.IntN(62))
		}
		if rng.IntN(3) == 0 {
			position := rng.IntN(len(text))
			text = text[:position] + string(alphabet[rng.IntN(len(alphabet))]) + text[position+1:]
		}
		wantSec, wantNanos, wantOK := goLeaseInstant(text)
		gotSec, gotNanos, gotOK := referenceLeaseInstant(text, true)
		if gotOK != wantOK || (wantOK && (gotSec != wantSec || gotNanos != wantNanos)) {
			t.Fatalf("%q: arithmetic = (%d,%d,%t), Go decode = (%d,%d,%t)", text, gotSec, gotNanos, gotOK, wantSec, wantNanos, wantOK)
		}
	}
}

func TestLeaseInstantSQLShape(t *testing.T) {
	for name, dialect := range map[string]LeaseSQLDialect{"singlestore": SingleStoreLeaseSQL, "postgres": PostgresLeaseSQL} {
		sql := dialect.LeaseInstantSQL("SELECT id,lease_text,lease_string FROM src", "id")
		// A bind placeholder inside the SQL text would be read as an argument.
		if strings.ContainsAny(sql, "?") {
			t.Errorf("%s lease SQL contains a question mark", name)
		}
		for _, banned := range []string{"STR_TO_DATE", "TO_TIMESTAMP", "::timestamp", "UNIX_TIMESTAMP", "TIMESTAMP(", "CONVERT_TZ"} {
			if strings.Contains(strings.ToUpper(sql), strings.ToUpper(banned)) {
				t.Errorf("%s lease SQL uses the vendor date conversion %s", name, banned)
			}
		}
		for _, column := range []string{"AS lease_ok", "AS lease_sec", "AS lease_nanos"} {
			if !strings.Contains(sql, column) {
				t.Errorf("%s lease SQL does not expose %s", name, column)
			}
		}
	}
}
