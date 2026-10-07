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
	kindNull kind = iota
	kindBool
	kindNumber
	kindText
	kindTime
	kindMap
	kindList
	kindOther
	kindBad // a value that can't be compared (NaN, an invalid agentsafe.Decimal)
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

var null = value{k: kindNull, show: "null"}

// norm turns a Go value into a comparable value: concrete types first, then by reflect kind.
func norm(v any) value {
	switch x := v.(type) {
	case nil:
		return null
	case bool:
		return value{k: kindBool, b: x, show: strconv.FormatBool(x)}
	case string:
		return value{k: kindText, s: x, show: strconv.Quote(x) + " (text)"}
	case agentsafe.Decimal:
		return decimal(string(x), string(x)+" (agentsafe.Decimal)")
	case json.Number:
		return decimal(string(x), string(x))
	case float64:
		return float(x, 64)
	case float32:
		return float(float64(x), 32)
	case time.Time:
		return value{k: kindTime, t: x, show: x.Format(time.RFC3339Nano)}
	}
	return normReflect(v, reflect.ValueOf(v))
}

// normReflect handles named types (a type Cents int64), pointers and containers.
func normReflect(v any, rv reflect.Value) value {
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value{k: kindNumber, n: new(big.Rat).SetInt64(rv.Int()), show: strconv.FormatInt(rv.Int(), 10)}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := new(big.Int).SetUint64(rv.Uint())
		return value{k: kindNumber, n: new(big.Rat).SetInt(n), show: n.String()}
	case reflect.Float32, reflect.Float64:
		return float(rv.Float(), rv.Type().Bits())
	case reflect.String: // named string types (other than agentsafe.Decimal) are text
		return norm(rv.String())
	case reflect.Bool:
		return norm(rv.Bool())
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return null
		}
		return norm(rv.Elem().Interface())
	case reflect.Map:
		return normMap(v, rv)
	case reflect.Slice, reflect.Array:
		return normList(v, rv)
	default: // structs, channels, funcs, complex numbers: compared as opaque values of one type
		return value{k: kindOther, raw: v, show: fmt.Sprintf("%v (%T)", v, v)}
	}
}

func normMap(v any, rv reflect.Value) value {
	if rv.IsNil() {
		return null
	}
	m := make(map[string]value, rv.Len())
	for it := rv.MapRange(); it.Next(); {
		m[fmt.Sprint(it.Key().Interface())] = norm(it.Value().Interface())
	}
	return value{k: kindMap, m: m, show: fmt.Sprint(v)}
}

func normList(v any, rv reflect.Value) value {
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return null
	}
	l := make([]value, rv.Len())
	for i := range l {
		l[i] = norm(rv.Index(i).Interface())
	}
	return value{k: kindList, l: l, show: fmt.Sprint(v)}
}

func decimal(s, show string) value {
	n, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || strings.ContainsAny(s, "/") { // SetString also accepts "1/3": not a decimal
		return value{k: kindBad, show: show + ": not a decimal number"}
	}
	return value{k: kindNumber, n: n, show: show}
}

func float(f float64, bits int) value {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return value{k: kindBad, show: strconv.FormatFloat(f, 'g', -1, bits) + ": not a number"}
	}
	s := strconv.FormatFloat(f, 'g', -1, bits)
	n, _ := new(big.Rat).SetString(s)
	return value{k: kindNumber, n: n, show: s}
}

var kindNames = map[kind]string{kindNull: "null", kindBool: "bool", kindNumber: "number", kindText: "text",
	kindTime: "time", kindMap: "object", kindList: "list", kindOther: "other", kindBad: "invalid"}

// same compares two normalized values: containers element by element, everything else as one value.
func same(w, g value) (bool, string) {
	if w.k == kindBad || g.k == kindBad {
		return false, fmt.Sprintf("%s vs %s: can't be compared", w.show, g.show)
	}
	if w.k != g.k {
		return false, fmt.Sprintf("%s is %s, should be %s %s", g.show, kindNames[g.k], kindNames[w.k], w.show)
	}
	if w.k == kindMap {
		return sameMap(w.m, g.m)
	}
	if w.k == kindList {
		return sameList(w.l, g.l)
	}
	if !sameScalar(w, g) {
		return false, fmt.Sprintf("%s, should be %s", g.show, w.show)
	}
	return true, ""
}

// sameScalar compares two non-container values of one kind.
func sameScalar(w, g value) bool {
	switch w.k {
	case kindNull:
		return true
	case kindBool:
		return w.b == g.b
	case kindNumber:
		return w.n.Cmp(g.n) == 0
	case kindText:
		return w.s == g.s
	case kindTime:
		return w.t.Equal(g.t)
	case kindOther:
		return reflect.TypeOf(w.raw) == reflect.TypeOf(g.raw) && reflect.DeepEqual(w.raw, g.raw)
	case kindBad, kindMap, kindList: // handled by same; listed so the switch stays exhaustive over kinds
	}
	return false
}

// sameMap compares key by key in sorted order, so the first difference reported is deterministic.
func sameMap(w, g map[string]value) (bool, string) {
	keys := make([]string, 0, len(w)+len(g))
	for k := range w {
		keys = append(keys, k)
	}
	for k := range g {
		if _, inW := w[k]; !inW {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		wv, inW := w[k]
		gv, inG := g[k]
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
}

func sameList(w, g []value) (bool, string) {
	if len(w) != len(g) {
		return false, fmt.Sprintf("%d items, should be %d", len(g), len(w))
	}
	for i := range w {
		if eq, why := same(w[i], g[i]); !eq {
			return false, fmt.Sprintf("[%d]: %s", i, why)
		}
	}
	return true, ""
}
