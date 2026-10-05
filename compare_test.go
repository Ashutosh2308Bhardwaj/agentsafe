package agentsafe

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

type rupees float64 // a named numeric type, as user code often has
type status string

func TestSameNumbersCompareByExactValueAcrossTypes(t *testing.T) {
	f := 4200.5
	for _, c := range []struct{ a, b any }{
		{4200, 4200.0}, {int64(4200), uint16(4200)}, {4200, json.Number("4200")}, {Decimal("4200.50"), 4200.5},
		{Decimal("4200.50"), Decimal("4200.5")}, {Decimal("0.1"), 0.1}, {rupees(99.99), Decimal("99.99")},
		{float32(0.1), Decimal("0.1")}, {&f, Decimal("4200.5")}, {Decimal(" 7 "), 7},
		{uint64(math.MaxUint64), json.Number("18446744073709551615")},
	} {
		if eq, why := Same(c.a, c.b); !eq {
			t.Errorf("Same(%#v, %#v) = false (%s), want true", c.a, c.b, why)
		}
	}
}

func TestSameRefusesWhatOnlyLooksEqual(t *testing.T) {
	// Variables, not constants: Go evaluates 0.1+0.2 between constants exactly, at compile time (= 0.3).
	tenth, fifth := 0.1, 0.2
	for _, c := range []struct {
		a, b any
		why  string
	}{
		{4200, "4200", `"4200" (text) is text, should be number 4200`}, // an amount stored as text
		{"4200", 4200, "4200 is number, should be text"},
		{nil, "<nil>", "is text, should be null"}, // Sprint(nil) == "<nil>": no longer equal
		{nil, 0, "0 is number, should be null"},
		{0.3, tenth + fifth, "0.30000000000000004, should be 0.3"}, // float drift is reported, not tolerated
		{Decimal("4200.50"), Decimal("4200.51"), "4200.51 (Decimal), should be 4200.50 (Decimal)"},
		{Decimal("12,500"), 12500, "not a decimal number"},
		{Decimal("1/3"), 1, "not a decimal number"},
		{math.NaN(), math.NaN(), "not a number"},
		{math.Inf(1), math.Inf(1), "not a number"},
		{true, "true", "is text, should be bool"},
		{status("paid"), "paid ", `"paid " (text), should be "paid" (text)`},
		{[]any{1, 2}, []any{1}, "1 items, should be 2"},
		{[]any{1, "2"}, []any{1, 2}, "[1]: 2 is number, should be text"},
		{map[string]any{"a": 1}, map[string]any{"a": 1, "b": 2}, ".b = 2 shouldn't be there"},
		{map[string]any{"a": 1, "b": nil}, map[string]any{"a": 1}, ".b is missing (should be null)"},
		{map[string]any{"amt": Decimal("10.00")}, map[string]any{"amt": "10.00"}, ".amt: \"10.00\" (text) is text"},
	} {
		eq, why := Same(c.a, c.b)
		if eq || !strings.Contains(why, c.why) {
			t.Errorf("Same(%#v, %#v) = %v %q, want false with %q", c.a, c.b, eq, why, c.why)
		}
	}
}

func TestSameNullsTimesAndOtherTypes(t *testing.T) {
	var np *float64
	var nm map[string]any
	var ns []int
	ist, _ := time.LoadLocation("Asia/Kolkata")
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	type point struct{ X, Y int }
	for _, c := range []struct{ a, b any }{
		{nil, np}, {nil, nm}, {nil, ns}, {at, at.In(ist)}, {point{1, 2}, point{1, 2}},
		{[]int{1, 2}, []any{1.0, json.Number("2")}}, {map[string]int{"a": 1}, map[string]any{"a": Decimal("1")}},
	} {
		if eq, why := Same(c.a, c.b); !eq {
			t.Errorf("Same(%#v, %#v) = false (%s)", c.a, c.b, why)
		}
	}
	type other struct{ X, Y int }
	for _, c := range []struct{ a, b any }{{at, at.Add(time.Nanosecond)}, {point{1, 2}, point{1, 3}}, {point{1, 2}, other{1, 2}}} {
		if eq, _ := Same(c.a, c.b); eq {
			t.Errorf("Same(%#v, %#v) = true", c.a, c.b)
		}
	}
}

func TestReconcileUsesTypedComparison(t *testing.T) {
	exp := []Effect{{ID: "p", Fields: map[string]any{"amount": Decimal("11000.00"), "payee": "Ramesh", "note": nil}}}
	ok := []Effect{{ID: "p", Fields: map[string]any{"amount": 11000.0, "payee": "Ramesh", "note": nil, "extra_column": 1}}}
	if rep := Reconcile(exp, ok, nil, nil); exactFindings(rep) != 0 || rep.Matched != 1 {
		t.Fatalf("same values in different Go types must match; extra columns are ignored: %v", rep)
	}
	for name, got := range map[string]map[string]any{
		"amount as text": {"amount": "11000", "payee": "Ramesh", "note": nil},
		"missing note":   {"amount": 11000, "payee": "Ramesh"},
		"short by paisa": {"amount": 10999.99, "payee": "Ramesh", "note": nil},
	} {
		rep := Reconcile(exp, []Effect{{ID: "p", Fields: got}}, nil, nil)
		if exactFindings(rep) != 1 {
			t.Errorf("%s: want an exact-check finding, got %v", name, rep)
		}
	}
	// Claims too: the model said "5" in text; the records hold the number 5.
	rep := Reconcile(nil, nil, nil, []Claim{{Name: "TOTAL", Claimed: "5", Actual: 5}})
	if rep.Pass() || !strings.Contains(rep.Findings[0].Detail, "is text, should be number") {
		t.Fatalf("a claim must be parsed into the records' type before it's compared: %v", rep)
	}
	if rep := Reconcile(nil, nil, nil, []Claim{{Name: "TOTAL", Claimed: 5, Actual: int64(5)}}); !rep.Pass() {
		t.Fatalf("got %v", rep)
	}
}

// exactFindings counts field-comparison findings (these tests have no log, so provenance warnings are expected).
func exactFindings(rep Report) int {
	n := 0
	for _, f := range rep.Findings {
		if f.Check == "exact" || f.Check == "complete" {
			n++
		}
	}
	return n
}
