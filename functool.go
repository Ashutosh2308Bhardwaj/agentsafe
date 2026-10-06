package agentsafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Func turns an ordinary Go function into a Tool. The model's arguments are decoded into In (strictly: an
// unknown field is an error the model sees), the JSON schema the model is shown is generated from In, and the
// safety features are options:
//
//	type Payout struct {
//		InvoiceID string            `json:"invoice_id" desc:"the invoice being paid"`
//		Payee     string            `json:"payee"`
//		Amount    agentsafe.Decimal `json:"amount" desc:"in rupees, e.g. 4200.50"`
//	}
//	pay := agentsafe.Func("send_payout", "Pay an approved invoice", sendPayout,
//		agentsafe.Idempotent("invoice_id"),                        // one payout per invoice, ever
//		agentsafe.NeedsApproval(func(p Payout) any { return p }),  // a human approves first
//		agentsafe.Check(checkAgainstLedger),                       // grounded before anyone is asked
//		agentsafe.Timeout(10*time.Second))
//
// With Idempotent, fn gets the operation's key from KeyFrom(ctx): pass it to the system you call (as an
// Idempotency-Key header, a unique column) so a retry after a crash or timeout can't act twice.
//
// Schema: exported fields named by their json tag; a field is required unless it is a pointer or tagged
// omitempty; `desc:"..."` describes it; `enum:"a,b"` restricts a string. Decimal is a decimal string;
// time.Time an RFC 3339 string.
//
// Configuration mistakes (an approval on a tool that isn't Idempotent, an identity field In doesn't have, an
// option written for another input type) are reported by New/Validate as ErrConfig.
func Func[In, Out any](name, description string, fn func(ctx context.Context, in In) (Out, error), opts ...FuncOption) Tool {
	in := reflect.TypeFor[In]()
	f := &funcTool{name: name, description: description, in: in}
	f.call = func(ctx context.Context, v any) (any, error) { return fn(ctx, v.(In)) }
	f.schema, f.err = schemaOf(in)
	f.fields = jsonFields(in)
	for _, o := range opts {
		if o.in != nil && o.in != in {
			f.errf("option %s is for input type %s, not %s", o.name, o.in, in)
			continue
		}
		o.apply(f)
	}
	if f.needsApproval != nil && f.identity == nil {
		f.errf("NeedsApproval requires Idempotent: approvals are addressed by operation key")
	}
	switch {
	case f.identity != nil && f.needsApproval != nil:
		return &gatedFuncTool{idempotentFuncTool{f}}
	case f.identity != nil:
		return &idempotentFuncTool{f}
	}
	return f
}

// KeyFrom returns the idempotency key of the operation being executed, inside a Func made Idempotent.
func KeyFrom(ctx context.Context) string {
	k, _ := ctx.Value(keyCtx{}).(string)
	return k
}

type keyCtx struct{}

// FuncOption configures a Func.
type FuncOption struct {
	name  string
	in    reflect.Type // the input type a typed option was written for; nil = any
	apply func(*funcTool)
}

// Idempotent makes the tool act at most once per operation. The operation is identified by these fields of
// the input (business identity, e.g. "invoice_id"); every other field is the payload, compared on replay: the
// same identity with a different payload is a conflict the model is told about, not a second action.
func Idempotent(fields ...string) FuncOption {
	return FuncOption{name: "Idempotent", apply: func(f *funcTool) {
		if len(fields) == 0 {
			f.errf("Idempotent needs at least one identity field")
		}
		for _, n := range fields {
			if !slices.Contains(f.fields, n) {
				f.errf("Idempotent: input has no field %q (fields: %s)", n, strings.Join(f.fields, ", "))
			}
		}
		f.identity = fields
	}}
}

// NeedsApproval makes every call wait for a human decision (Runner.Approve / Reject). summary is what the
// approver is shown; it receives the arguments after Check has passed.
func NeedsApproval[In any](summary func(In) any) FuncOption {
	return ApprovalIf(func(In) bool { return true }, summary)
}

// ApprovalIf is NeedsApproval for some calls only, e.g. payouts above a threshold.
func ApprovalIf[In any](when func(In) bool, summary func(In) any) FuncOption {
	return FuncOption{name: "NeedsApproval", in: reflect.TypeFor[In](), apply: func(f *funcTool) {
		f.needsApproval = func(v any) bool { return when(v.(In)) }
		f.summary = func(v any) any { return summary(v.(In)) }
	}}
}

// Check validates the arguments against your source of truth before anything runs or anyone is asked to
// approve. Its error goes back to the model, which can correct itself; nothing is executed.
func Check[In any](check func(ctx context.Context, in In) error) FuncOption {
	return FuncOption{name: "Check", in: reflect.TypeFor[In](), apply: func(f *funcTool) {
		f.check = func(ctx context.Context, v any) error { return check(ctx, v.(In)) }
	}}
}

// Timeout bounds each call of this tool, overriding the Runner's default.
func Timeout(d time.Duration) FuncOption {
	return FuncOption{name: "Timeout", apply: func(f *funcTool) {
		if d <= 0 {
			f.errf("Timeout must be positive")
		}
		f.timeout = d
	}}
}

type funcTool struct {
	name, description string
	in                reflect.Type
	schema            json.RawMessage
	fields            []string // top-level JSON field names of In
	call              func(context.Context, any) (any, error)

	identity      []string
	needsApproval func(any) bool
	summary       func(any) any
	check         func(context.Context, any) error
	timeout       time.Duration
	err           error
}

func (f *funcTool) errf(format string, a ...any) {
	f.err = errors.Join(f.err, fmt.Errorf("tool %s: "+format, append([]any{f.name}, a...)...))
}

func (f *funcTool) configErr() error { return f.err }

// wantsApproval: asked for NeedsApproval, even if misconfigured, so Validate reports a missing approval
// policy in the same pass as the misconfiguration.
func (f *funcTool) wantsApproval() bool { return f.needsApproval != nil }

func (f *funcTool) Spec() ToolSpec {
	return ToolSpec{Name: f.name, Description: f.description, Parameters: f.schema}
}

// decode reads the model's arguments into a new In, refusing unknown fields and trailing data.
func (f *funcTool) decode(args json.RawMessage) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	p := reflect.New(f.in)
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	if err := d.Decode(p.Interface()); err != nil {
		return nil, fmt.Errorf("arguments don't match the %s schema: %w", f.name, err)
	}
	if d.More() {
		return nil, fmt.Errorf("arguments don't match the %s schema: trailing data", f.name)
	}
	return p.Elem().Interface(), nil
}

func (f *funcTool) Call(ctx context.Context, args json.RawMessage) (any, error) {
	v, err := f.decode(args)
	if err != nil {
		return nil, err
	}
	return f.call(ctx, v)
}

// Validate implements Validator.
func (f *funcTool) Validate(ctx context.Context, args json.RawMessage) error {
	v, err := f.decode(args)
	if err != nil || f.check == nil {
		return err
	}
	return f.check(ctx, v)
}

// Timeout implements TimeoutTool (0 = the Runner's default).
func (f *funcTool) Timeout() time.Duration { return f.timeout }

type idempotentFuncTool struct{ *funcTool }

// Identity implements IdempotentTool: the identity fields, and the whole arguments as payload.
func (f *idempotentFuncTool) Identity(args json.RawMessage) (any, any, error) {
	if _, err := f.decode(args); err != nil {
		return nil, nil, err
	}
	var all map[string]any
	if err := json.Unmarshal(args, &all); err != nil {
		return nil, nil, err
	}
	id := map[string]any{}
	for _, n := range f.identity {
		v, ok := all[n]
		if !ok || v == nil || v == "" {
			return nil, nil, fmt.Errorf("%s: identity field %q is missing", f.name, n)
		}
		id[n] = v
	}
	return id, all, nil
}

// CallWithKey implements IdempotentTool: fn reads the key with KeyFrom(ctx).
func (f *idempotentFuncTool) CallWithKey(ctx context.Context, key string, args json.RawMessage) (any, error) {
	return f.Call(context.WithValue(ctx, keyCtx{}, key), args)
}

type gatedFuncTool struct{ idempotentFuncTool }

// NeedsApproval implements Gated.
func (f *gatedFuncTool) NeedsApproval(args json.RawMessage) bool {
	v, err := f.decode(args)
	return err != nil || f.needsApproval(v) // undecodable: never skip the gate on a parse error
}

// Summary implements Gated.
func (f *gatedFuncTool) Summary(args json.RawMessage) (any, error) {
	v, err := f.decode(args)
	if err != nil {
		return nil, err
	}
	return f.summary(v), nil
}

// ---- schema

// jsonFields lists a struct's top-level JSON field names, by the same rules as the schema.
func jsonFields(t reflect.Type) []string {
	var out []string
	if t.Kind() != reflect.Struct {
		return nil
	}
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() || sf.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		out = append(out, name)
	}
	return out
}

func schemaOf(t reflect.Type) (json.RawMessage, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("a Func's input must be a struct (the model sends a JSON object), not %s", t)
	}
	s, err := typeSchema(t, map[reflect.Type]bool{})
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	return b, err
}

var (
	decimalType = reflect.TypeFor[Decimal]()
	timeType    = reflect.TypeFor[time.Time]()
	rawType     = reflect.TypeFor[json.RawMessage]()
	numberType  = reflect.TypeFor[json.Number]()
)

func typeSchema(t reflect.Type, seen map[reflect.Type]bool) (map[string]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case decimalType:
		return map[string]any{"type": "string", "pattern": `^-?[0-9]+(\.[0-9]+)?$`, "description": "an exact decimal number, as text"}, nil
	case timeType:
		return map[string]any{"type": "string", "format": "date-time"}, nil
	case rawType:
		return map[string]any{}, nil
	case numberType:
		return map[string]any{"type": "number"}, nil
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice, reflect.Array:
		items, err := typeSchema(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map keys must be strings in a tool input (%s)", t)
		}
		v, err := typeSchema(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": v}, nil
	case reflect.Struct:
		return structSchema(t, seen)
	default: // channels, funcs, interfaces, complex numbers: nothing a model can send as JSON
		return nil, fmt.Errorf("type %s can't be a tool input", t)
	}
}

// structSchema is an object schema: exported fields by their JSON names, required unless optional.
func structSchema(t reflect.Type, seen map[reflect.Type]bool) (map[string]any, error) {
	if seen[t] {
		return nil, fmt.Errorf("recursive type %s can't be a tool input", t)
	}
	seen[t] = true
	defer delete(seen, t)
	props, required := map[string]any{}, []string{}
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() || sf.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		p, err := typeSchema(sf.Type, seen)
		if err != nil {
			return nil, err
		}
		if d := sf.Tag.Get("desc"); d != "" {
			p["description"] = d
		}
		if e := sf.Tag.Get("enum"); e != "" {
			p["enum"] = strings.Split(e, ",")
		}
		props[name] = p
		if sf.Type.Kind() != reflect.Pointer && !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}, nil
}
