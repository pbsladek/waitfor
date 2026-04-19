package expr

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluateJSON(t *testing.T) {
	body := []byte(`{"ready":true,"status":"ok","count":12,"items":[{"name":"first"}],"nested":{"value":3}}`)

	tests := []struct {
		name string
		expr string
		want bool
	}{
		{name: "truthy bool", expr: ".ready", want: true},
		{name: "string equality", expr: `.status == "ok"`, want: true},
		{name: "numeric greater", expr: ".count >= 10", want: true},
		{name: "array index", expr: `.items[0].name == "first"`, want: true},
		{name: "kubernetes style", expr: "{.status}=ok", want: true},
		{name: "not satisfied", expr: ".nested.value > 10", want: false},
		{name: "missing", expr: ".missing", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expression, err := Compile(tt.expr)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}
			got, _, err := expression.EvaluateJSON(body)
			if err != nil {
				t.Fatalf("EvaluateJSON() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("EvaluateJSON() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluateJSONRejectsInvalidPath(t *testing.T) {
	expression, err := Compile("ready == true")
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, _, err = expression.EvaluateJSON([]byte(`{"ready":true}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEvaluateDetailsDoNotExposeValues(t *testing.T) {
	body := []byte(`{"token":"super-secret","ready":true}`)

	truthyExpr := MustCompile(".token")
	_, detail, err := truthyExpr.EvaluateJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, "super-secret") {
		t.Fatalf("detail = %q, want secret value redacted", detail)
	}

	compareExpr := MustCompile(".token == super-secret")
	_, detail, err = compareExpr.EvaluateJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, "super-secret") {
		t.Fatalf("detail = %q, want expected value redacted", detail)
	}
}

func TestCompareOperators(t *testing.T) {
	body := []byte(`{"n":5,"s":"hello","b":false,"null_val":null}`)
	tests := []struct {
		expr string
		want bool
	}{
		{".n != 3", true},
		{".n <= 5", true},
		{".n < 6", true},
		{".n > 4", true},
		{".n >= 5", true},
		{`.s != "world"`, true},
		{`.b == false`, true},
		{`.b != true`, true},
		{`.null_val == null`, true},
		{`.null_val != null`, false},
		{`.n == 5`, true},
		{`.n == 6`, false},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			e, err := Compile(tt.expr)
			if err != nil {
				t.Fatalf("Compile(%q) error = %v", tt.expr, err)
			}
			got, _, err := e.EvaluateJSON(body)
			if err != nil {
				t.Fatalf("EvaluateJSON(%q) error = %v", tt.expr, err)
			}
			if got != tt.want {
				t.Fatalf("EvaluateJSON(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestTruthy(t *testing.T) {
	tests := []struct {
		v    any
		want bool
	}{
		{nil, false},
		{true, true},
		{false, false},
		{"hello", true},
		{"", false},
		{float64(1), true},
		{float64(0), false},
		{map[string]any{"k": "v"}, true},
		{map[string]any{}, false},
		{[]any{1}, true},
		{[]any{}, false},
	}
	for _, tt := range tests {
		if got := truthy(tt.v); got != tt.want {
			t.Fatalf("truthy(%v) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestParseLiteral(t *testing.T) {
	tests := []struct {
		raw  string
		want any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"3.14", float64(3.14)},
		{"42", float64(42)},
		{"hello", "hello"},
		{`"quoted"`, "quoted"},
	}
	for _, tt := range tests {
		got, err := parseLiteral(tt.raw)
		if err != nil {
			t.Fatalf("parseLiteral(%q) error = %v", tt.raw, err)
		}
		if got != tt.want {
			t.Fatalf("parseLiteral(%q) = %v (%T), want %v (%T)", tt.raw, got, got, tt.want, tt.want)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []string{
		"",
		"  ",
		".field == ",
		" == value",
	}
	for _, expr := range tests {
		if _, err := Compile(expr); err == nil {
			t.Fatalf("Compile(%q) expected error, got nil", expr)
		}
	}
}

func TestMustCompilePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustCompile with invalid expr should panic")
		}
	}()
	MustCompile("")
}

func TestExpressionString(t *testing.T) {
	e := MustCompile(".status == ok")
	if e.String() != ".status == ok" {
		t.Fatalf("String() = %q, want %q", e.String(), ".status == ok")
	}
}

func TestAsFloat(t *testing.T) {
	tests := []struct {
		v    any
		want float64
		ok   bool
	}{
		{float64(3.14), 3.14, true},
		{float32(2.5), float64(float32(2.5)), true},
		{int(7), 7, true},
		{int64(100), 100, true},
		{"not a number", 0, false},
		{json.Number("42.5"), 42.5, true},
		{json.Number("bad"), 0, false},
	}
	for _, tt := range tests {
		got, ok := asFloat(tt.v)
		if ok != tt.ok {
			t.Fatalf("asFloat(%v) ok = %v, want %v", tt.v, ok, tt.ok)
		}
		if ok && got != tt.want {
			t.Fatalf("asFloat(%v) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestTruthyReflectDefault(t *testing.T) {
	// An int (not one of the typed cases) hits the reflect.ValueOf branch
	if !truthy(int(1)) {
		t.Fatal("truthy(int(1)) = false, want true")
	}
	if truthy(int(0)) {
		t.Fatal("truthy(int(0)) = true, want false")
	}
}

func TestSplitJSONPathPartTrailingGarbage(t *testing.T) {
	// "field[0]extra" — text after "]" that isn't "[" triggers an error
	_, _, err := splitJSONPathPart("field[0]extra")
	if err == nil {
		t.Fatal("expected error for trailing text after ]")
	}
}

func TestLookupJSONPathErrors(t *testing.T) {
	doc := map[string]any{"items": []any{1, 2}}

	// no leading dot
	_, _, err := lookupJSONPath(doc, "items")
	if err == nil {
		t.Fatal("expected error for path without leading dot")
	}

	// bad index syntax
	_, _, err = lookupJSONPath(doc, ".items[bad]")
	if err == nil {
		t.Fatal("expected error for non-numeric index")
	}
}

func TestEvaluateJSONBadJSON(t *testing.T) {
	e := MustCompile(".field")
	_, _, err := e.EvaluateJSON([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestNullCompareOrdering(t *testing.T) {
	body := []byte(`{"n":null}`)
	e := MustCompile(".n >= 0")
	_, _, err := e.EvaluateJSON(body)
	if err == nil {
		t.Fatal("expected error comparing null with >=")
	}
}

func TestBoolOrderingUnsupported(t *testing.T) {
	body := []byte(`{"b":true}`)
	// bool compared with >= against a non-bool returns false (not an error) since
	// want parses as float64 and bool→float conversion is not supported.
	e := MustCompile(".b >= true")
	_, _, err := e.EvaluateJSON(body)
	if err == nil {
		t.Fatal("expected error using >= on bool with bool operand")
	}
}

func TestStringOrdering(t *testing.T) {
	body := []byte(`{"s":"hello"}`)
	e := MustCompile(".s > a")
	_, _, err := e.EvaluateJSON(body)
	if err == nil {
		t.Fatal("expected error using > on string")
	}
}
