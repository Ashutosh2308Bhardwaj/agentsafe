package reconcile

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// Same reports whether two field values are equal for reconciliation, and if not, why. It compares by kind:
//
//	numbers   every Go int, uint and float type, json.Number and agentsafe.Decimal compare by exact decimal value, so
//	          4200 == 4200.0 == agentsafe.Decimal("4200.00"). A float is taken at its shortest decimal form (what it
//	          prints as): 0.1 == agentsafe.Decimal("0.1"), but 0.1+0.2 (0.30000000000000004) != 0.3. Arithmetic drift
//	          is reported, never tolerated: keep money in agentsafe.Decimal or integer minor units. NaN and ±Inf never match.
//	text      equal only to identical text. "4200" is NOT 4200: a system that stored an amount as text is a
//	          finding, not a match.
//	null      nil, a nil pointer, map or slice. Equal only to null.
//	time      time.Time, by instant (time zones don't matter).
//	maps, lists   element by element, recursively; map keys compared as text.
//	pointers  compared by what they point to.
//	anything else: same type and reflect.DeepEqual.
func Same(want, got any) (bool, string) {
	return same(norm(want), norm(got))
}

type kind int

const (
	kNull kind = iota
	kBool
	kNumber
	kText
	kTime
	kMap
	kList
	kOther
	kBad // a value that can't be compared (NaN, an invalid agentsafe.Decimal)
)

type value struct {
	k    kind
	b    bool
	n    *big.Rat
	s    string
	t    time.Time
	m    map[string]value
	l    []value
	raw  any
	show string
}

func norm(v any) value {
	switch x := v.(type) {
	case nil:
		return value{k: kNull, show: "null"}
	case bool:
		return value{k: kBool, b: x, show: strconv.FormatBool(x)}
	case string:
		return value{k: kText, s: x, show: strconv.Quote(x) + " (text)"}
	case agentsafe.Decimal:
		return decimal(string(x), string(x)+" (agentsafe.Decimal)")
	case json.Number:
		return decimal(string(x), string(x))
	case float64:
		return float(x, 64)
	case float32:
		return float(float64(x), 32)
	case time.Time:
		return value{k: kTime, t: x, show: x.Format(time.RFC3339Nano)}
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value{k: kNumber, n: new(big.Rat).SetInt64(rv.Int()), show: strconv.FormatInt(rv.Int(), 10)}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := new(big.Int).SetUint64(rv.Uint())
		return value{k: kNumber, n: new(big.Rat).SetInt(n), show: n.String()}
	case reflect.Float32, reflect.Float64: // named float types
		return float(rv.Float(), rv.Type().Bits())
	case reflect.String: // named string types (other than agentsafe.Decimal) are text
		return norm(rv.String())
	case reflect.Bool:
		return norm(rv.Bool())
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return value{k: kNull, show: "null"}
		}
		return norm(rv.Elem().Interface())
	case reflect.Map:
		if rv.IsNil() {
			return value{k: kNull, show: "null"}
		}
		m := make(map[string]value, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			m[fmt.Sprint(it.Key().Interface())] = norm(it.Value().Interface())
		}
		return value{k: kMap, m: m, show: fmt.Sprint(v)}
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return value{k: kNull, show: "null"}
		}
		l := make([]value, rv.Len())
		for i := range l {
			l[i] = norm(rv.Index(i).Interface())
		}
		return value{k: kList, l: l, show: fmt.Sprint(v)}
	default: // structs, channels, funcs, complex numbers: compared as opaque values of one type
		return value{k: kOther, raw: v, show: fmt.Sprintf("%v (%T)", v, v)}
	}
}

func decimal(s, show string) value {
	n, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || strings.ContainsAny(s, "/") { // SetString also accepts "1/3": not a decimal
		return value{k: kBad, show: show + ": not a decimal number"}
	}
	return value{k: kNumber, n: n, show: show}
}

func float(f float64, bits int) value {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return value{k: kBad, show: strconv.FormatFloat(f, 'g', -1, bits) + ": not a number"}
	}
	s := strconv.FormatFloat(f, 'g', -1, bits)
	n, _ := new(big.Rat).SetString(s)
	return value{k: kNumber, n: n, show: s}
}

var kindNames = map[kind]string{kNull: "null", kBool: "bool", kNumber: "number", kText: "text", kTime: "time",
	kMap: "object", kList: "list", kOther: "other", kBad: "invalid"}

func same(w, g value) (bool, string) {
	if w.k == kBad || g.k == kBad {
		return false, fmt.Sprintf("%s vs %s: can't be compared", w.show, g.show)
	}
	if w.k != g.k {
		return false, fmt.Sprintf("%s is %s, should be %s %s", g.show, kindNames[g.k], kindNames[w.k], w.show)
	}
	ok := true
	switch w.k {
	case kBad: // refused above; listed so the switch stays exhaustive over kinds
		return false, w.show
	case kNull:
	case kBool:
		ok = w.b == g.b
	case kNumber:
		ok = w.n.Cmp(g.n) == 0
	case kText:
		ok = w.s == g.s
	case kTime:
		ok = w.t.Equal(g.t)
	case kMap:
		keys := map[string]bool{}
		for k := range w.m {
			keys[k] = true
		}
		for k := range g.m {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			wv, inW := w.m[k]
			gv, inG := g.m[k]
			switch {
			case !inG:
				return false, fmt.Sprintf(".%s is missing (should be %s)", k, wv.show)
			case !inW:
				return false, fmt.Sprintf(".%s = %s shouldn't be there", k, gv.show)
			}
			if eq, why := same(wv, gv); !eq {
				return false, "." + k + ": " + why
			}
		}
		return true, ""
	case kList:
		if len(w.l) != len(g.l) {
			return false, fmt.Sprintf("%d items, should be %d", len(g.l), len(w.l))
		}
		for i := range w.l {
			if eq, why := same(w.l[i], g.l[i]); !eq {
				return false, fmt.Sprintf("[%d]: %s", i, why)
			}
		}
		return true, ""
	case kOther:
		if reflect.TypeOf(w.raw) != reflect.TypeOf(g.raw) {
			return false, fmt.Sprintf("%s, should be %s", g.show, w.show)
		}
		ok = reflect.DeepEqual(w.raw, g.raw)
	}
	if !ok {
		return false, fmt.Sprintf("%s, should be %s", g.show, w.show)
	}
	return true, ""
}
