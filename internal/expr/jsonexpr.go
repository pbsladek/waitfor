package expr

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

type Expression struct {
	raw        string
	comparison jsonComparison
}

func (e *Expression) String() string { return e.raw }

type jsonComparison struct {
	path string
	op   string
	want string
}

// Compile supports a deliberately small JSONPath-like subset:
// .field, .field.subfield, .items[0].name, and comparisons with ==, !=, =,
// >=, <=, >, <. Kubernetes-style {.status.phase}=Running is also accepted.
func Compile(raw string) (*Expression, error) {
	cmp, err := parseJSONComparison(raw)
	if err != nil {
		return nil, err
	}
	return &Expression{raw: raw, comparison: cmp}, nil
}

// MustCompile compiles a JSONPath expression and panics on error.
// Intended for use in tests and package-level variables.
func MustCompile(raw string) *Expression {
	e, err := Compile(raw)
	if err != nil {
		panic("expr: MustCompile(" + raw + "): " + err.Error())
	}
	return e
}

func (e *Expression) EvaluateJSON(body []byte) (bool, string, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, "", fmt.Errorf("parse json: %w", err)
	}
	return e.Evaluate(doc)
}

func (e *Expression) Evaluate(doc any) (bool, string, error) {
	cmp := e.comparison
	got, ok, err := lookupJSONPath(doc, cmp.path)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "", nil
	}

	if cmp.op == "" {
		return truthy(got), fmt.Sprintf("%s is truthy", cmp.path), nil
	}

	want, err := parseLiteral(cmp.want)
	if err != nil {
		return false, "", err
	}
	matched, err := compareValues(got, want, cmp.op)
	if err != nil {
		return false, "", err
	}
	return matched, fmt.Sprintf("%s %s expected value", cmp.path, cmp.op), nil
}

func parseJSONComparison(expr string) (jsonComparison, error) {
	expr = strings.TrimSpace(expr)
	expr = strings.Trim(expr, "'\"")
	if expr == "" {
		return jsonComparison{}, fmt.Errorf("jsonpath expression is required")
	}

	operators := []string{">=", "<=", "==", "!=", ">", "<", "="}
	if idx, op := findOperator(expr, operators); idx >= 0 {
		path := strings.TrimSpace(expr[:idx])
		want := strings.TrimSpace(expr[idx+len(op):])
		if path == "" || want == "" {
			return jsonComparison{}, fmt.Errorf("invalid jsonpath comparison")
		}
		if op == "=" {
			op = "=="
		}
		return jsonComparison{path: normalizeJSONPath(path), op: op, want: want}, nil
	}

	return jsonComparison{path: normalizeJSONPath(expr)}, nil
}

func normalizeJSONPath(path string) string {
	return strings.Trim(strings.TrimSpace(path), "{}")
}

// findOperator scans s left-to-right and returns the index and value of the
// first occurrence of any operator. Longer operators take priority at each
// position, so ">=" is matched before ">" when both start at the same index.
func findOperator(s string, operators []string) (int, string) {
	for i := 0; i < len(s); i++ {
		for _, op := range operators {
			if strings.HasPrefix(s[i:], op) {
				return i, op
			}
		}
	}
	return -1, ""
}

func traverseField(cur any, field string) (any, bool) {
	obj, ok := cur.(map[string]any)
	if !ok {
		return nil, false
	}
	val, ok := obj[field]
	return val, ok
}

func traverseIndexes(cur any, indexes []int) (any, bool) {
	for _, idx := range indexes {
		arr, ok := cur.([]any)
		if !ok || idx < 0 || idx >= len(arr) {
			return nil, false
		}
		cur = arr[idx]
	}
	return cur, true
}

func lookupJSONPath(doc any, path string) (any, bool, error) {
	if !strings.HasPrefix(path, ".") {
		return nil, false, fmt.Errorf("jsonpath must start with '.' or '{.'")
	}
	cur := doc
	remaining := strings.TrimPrefix(path, ".")
	for remaining != "" {
		part, rest, _ := strings.Cut(remaining, ".")
		field, indexes, err := splitJSONPathPart(part)
		if err != nil {
			return nil, false, err
		}
		if field != "" {
			var ok bool
			cur, ok = traverseField(cur, field)
			if !ok {
				return nil, false, nil
			}
		}
		var ok bool
		cur, ok = traverseIndexes(cur, indexes)
		if !ok {
			return nil, false, nil
		}
		remaining = rest
	}
	return cur, true, nil
}

func splitJSONPathPart(part string) (string, []int, error) {
	field, rest, _ := strings.Cut(part, "[")
	var indexes []int
	for rest != "" {
		raw, tail, ok := strings.Cut(rest, "]")
		if !ok {
			return "", nil, fmt.Errorf("invalid jsonpath index")
		}
		idx, err := strconv.Atoi(raw)
		if err != nil {
			return "", nil, fmt.Errorf("invalid jsonpath index")
		}
		indexes = append(indexes, idx)
		rest = strings.TrimPrefix(tail, "[")
		if tail != "" && !strings.HasPrefix(tail, "[") {
			return "", nil, fmt.Errorf("invalid jsonpath segment")
		}
	}
	return field, indexes, nil
}

func parseLiteral(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "'\"")
	if raw == "true" {
		return true, nil
	}
	if raw == "false" {
		return false, nil
	}
	if raw == "null" {
		return nil, nil
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		return n, nil
	}
	return raw, nil
}

func compareValues(got any, want any, op string) (bool, error) {
	if got == nil || want == nil {
		return compareNullable(got, want, op)
	}
	if gn, ok := asFloat(got); ok {
		wn, ok := asFloat(want)
		if !ok {
			return false, nil
		}
		return compareFloat64(gn, wn, op)
	}
	if gb, ok := got.(bool); ok {
		wb, ok := want.(bool)
		if !ok {
			return false, nil
		}
		return compareBool(gb, wb, op)
	}
	return compareString(toString(got), toString(want), op)
}

func compareNullable(got, want any, op string) (bool, error) {
	switch op {
	case "==":
		return got == want, nil
	case "!=":
		return got != want, nil
	default:
		return false, fmt.Errorf("cannot compare null with %s", op)
	}
}

func compareFloat64(a, b float64, op string) (bool, error) {
	switch op {
	case "==":
		return a == b, nil
	case "!=":
		return a != b, nil
	case ">=":
		return a >= b, nil
	case "<=":
		return a <= b, nil
	case ">":
		return a > b, nil
	case "<":
		return a < b, nil
	default:
		return false, fmt.Errorf("operator %s is not supported for numbers", op)
	}
}

func compareBool(a, b bool, op string) (bool, error) {
	switch op {
	case "==":
		return a == b, nil
	case "!=":
		return a != b, nil
	default:
		return false, fmt.Errorf("operator %s is not supported for booleans", op)
	}
}

func compareString(a, b, op string) (bool, error) {
	switch op {
	case "==":
		return a == b, nil
	case "!=":
		return a != b, nil
	default:
		return false, fmt.Errorf("operator %s is only supported for numbers", op)
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		n, err := t.Float64()
		return n, err == nil
	default:
		return 0, false
	}
}

func truthy(v any) bool {
	if v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	default:
		return !reflect.ValueOf(v).IsZero()
	}
}
