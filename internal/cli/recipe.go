package cli

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
	"github.com/pbsladek/wait-for/internal/output"
	"github.com/pbsladek/wait-for/internal/runner"
	"go.yaml.in/yaml/v3"
)

type recipeFile struct {
	Timeout        string           `yaml:"timeout"`
	Interval       string           `yaml:"interval"`
	MaxInterval    string           `yaml:"max_interval"`
	Backoff        string           `yaml:"backoff"`
	Jitter         string           `yaml:"jitter"`
	AttemptTimeout string           `yaml:"attempt_timeout"`
	Successes      *int             `yaml:"successes"`
	StableFor      string           `yaml:"stable_for"`
	Output         string           `yaml:"output"`
	Mode           string           `yaml:"mode"`
	Verbose        *bool            `yaml:"verbose"`
	Conditions     []map[string]any `yaml:"conditions"`
	Guards         []map[string]any `yaml:"guards"`
}

func applyRecipeConfig(opts globalOptions) (globalOptions, []condition.Condition, error) {
	data, err := readFileLimit(opts.configFile, maxHTTPBodyFileBytes)
	if err != nil {
		return opts, nil, err
	}
	var recipe recipeFile
	if err := decodeRecipe(data, &recipe); err != nil {
		return opts, nil, fmt.Errorf("invalid recipe config: %w", err)
	}
	opts, err = applyRecipeOptions(opts, recipe)
	if err != nil {
		return opts, nil, err
	}
	segments, err := recipeSegments(recipe)
	if err != nil {
		return opts, nil, err
	}
	conditions, err := parseConditions(segments)
	if err != nil {
		return opts, nil, err
	}
	return opts, conditions, nil
}

func decodeRecipe(data []byte, recipe *recipeFile) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(recipe); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("recipe must contain exactly one YAML document")
	}
	return nil
}

func applyRecipeOptions(opts globalOptions, recipe recipeFile) (globalOptions, error) {
	var err error
	opts, err = applyRecipeTimingOptions(opts, recipe)
	if err != nil {
		return opts, err
	}
	if recipe.Backoff != "" && !opts.changed["backoff"] {
		opts.backoff = runner.Backoff(strings.ToLower(recipe.Backoff))
	}
	if recipe.Jitter != "" && !opts.changed["jitter"] {
		opts.jitter, err = parseJitter(recipe.Jitter)
		if err != nil {
			return opts, fmt.Errorf("invalid recipe jitter: %w", err)
		}
	}
	opts, err = applyRecipeStabilityOptions(opts, recipe)
	if err != nil {
		return opts, err
	}
	opts = applyRecipePresentationOptions(opts, recipe)
	if opts.maxInterval == 0 {
		opts.maxInterval = opts.interval
	}
	return validateRecipeOptions(opts)
}

func applyRecipeTimingOptions(opts globalOptions, recipe recipeFile) (globalOptions, error) {
	if err := applyRecipeDurationOption(recipe.Timeout, opts.changed["timeout"], &opts.timeout, "timeout"); err != nil {
		return opts, err
	}
	if err := applyRecipeDurationOption(recipe.Interval, opts.changed["interval"], &opts.interval, "interval"); err != nil {
		return opts, err
	}
	if err := applyRecipeDurationOption(recipe.MaxInterval, opts.changed["max-interval"], &opts.maxInterval, "max_interval"); err != nil {
		return opts, err
	}
	return opts, nil
}

func applyRecipeDurationOption(raw string, changed bool, target *time.Duration, name string) error {
	if raw == "" || changed {
		return nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid recipe %s: %w", name, err)
	}
	*target = value
	return nil
}

func applyRecipeStabilityOptions(opts globalOptions, recipe recipeFile) (globalOptions, error) {
	var err error
	if recipe.AttemptTimeout != "" && !opts.changed["attempt-timeout"] {
		opts.perAttemptTimeout, err = time.ParseDuration(recipe.AttemptTimeout)
		if err != nil {
			return opts, fmt.Errorf("invalid recipe attempt_timeout: %w", err)
		}
	}
	if recipe.Successes != nil && !opts.changed["successes"] {
		opts.requiredSuccesses = *recipe.Successes
	}
	if recipe.StableFor != "" && !opts.changed["stable-for"] {
		opts.stableFor, err = time.ParseDuration(recipe.StableFor)
		if err != nil {
			return opts, fmt.Errorf("invalid recipe stable_for: %w", err)
		}
	}
	return opts, nil
}

func applyRecipePresentationOptions(opts globalOptions, recipe recipeFile) globalOptions {
	if recipe.Output != "" && !opts.changed["output"] {
		opts.format = output.Format(strings.ToLower(recipe.Output))
	}
	if recipe.Mode != "" && !opts.changed["mode"] {
		opts.mode = runner.Mode(strings.ToLower(recipe.Mode))
	}
	if recipe.Verbose != nil && !opts.changed["verbose"] {
		opts.verbose = *recipe.Verbose
	}
	return opts
}

func validateRecipeOptions(opts globalOptions) (globalOptions, error) {
	switch opts.format {
	case output.FormatText, output.FormatJSON, output.FormatNDJSON:
	default:
		return opts, fmt.Errorf("invalid output format %q", opts.format)
	}
	switch opts.mode {
	case runner.ModeAll, runner.ModeAny:
	default:
		return opts, fmt.Errorf("invalid mode %q", opts.mode)
	}
	if err := validateGeneralOptions(opts); err != nil {
		return opts, err
	}
	return opts, nil
}

func recipeSegments(recipe recipeFile) ([]string, error) {
	var args []string
	for _, item := range recipe.Conditions {
		segment, err := recipeConditionSegment(item, false)
		if err != nil {
			return nil, err
		}
		args = appendConditionSegment(args, segment)
	}
	for _, item := range recipe.Guards {
		segment, err := recipeConditionSegment(item, true)
		if err != nil {
			return nil, err
		}
		args = appendConditionSegment(args, segment)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("recipe requires at least one condition")
	}
	return args, nil
}

func appendConditionSegment(args []string, segment []string) []string {
	if len(args) > 0 {
		args = append(args, "--")
	}
	return append(args, segment...)
}

func recipeConditionSegment(item map[string]any, forceGuard bool) ([]string, error) {
	if _, ok := item["args"]; ok {
		return recipeArgsConditionSegment(item, forceGuard)
	}
	backend, raw, err := recipeBackend(item)
	if err != nil {
		return nil, err
	}
	segment, err := recipeBackendSegment(backend, raw)
	if err != nil {
		return nil, err
	}
	if name, ok := stringValue(item["name"]); ok && backend != "process" {
		segment = append([]string{segment[0], "--name", name}, segment[1:]...)
	}
	if forceGuard || boolValue(item["guard"]) {
		segment = append([]string{"guard"}, segment...)
	}
	return segment, nil
}

func recipeArgsConditionSegment(item map[string]any, forceGuard bool) ([]string, error) {
	if err := validateRecipeConditionKeys(item); err != nil {
		return nil, err
	}
	return recipeArgsSegment(item["args"], forceGuard || boolValue(item["guard"]))
}

func recipeArgsSegment(rawArgs any, guard bool) ([]string, error) {
	segment, err := stringList(rawArgs)
	if err != nil {
		return nil, fmt.Errorf("recipe args must be a string list")
	}
	if guard {
		return append([]string{"guard"}, segment...), nil
	}
	return segment, nil
}

func recipeBackend(item map[string]any) (string, any, error) {
	if err := validateRecipeConditionKeys(item); err != nil {
		return "", nil, err
	}
	for key, value := range item {
		switch key {
		case "name", "guard", "args":
			continue
		default:
			return strings.ToLower(key), value, nil
		}
	}
	return "", nil, fmt.Errorf("recipe condition requires a backend")
}

func validateRecipeConditionKeys(item map[string]any) error {
	count := 0
	for key := range item {
		if key != "name" && key != "guard" {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("recipe condition requires exactly one backend or args")
	}
	return nil
}

func recipeBackendSegment(backend string, raw any) ([]string, error) {
	if scalar, ok := scalarString(raw); ok {
		return []string{backend, scalar}, nil
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("recipe backend %q must be a scalar or map", backend)
	}
	if backend == "exec" {
		return recipeExecSegment(values)
	}
	segment := []string{backend}
	if target, ok := recipeTarget(backend, values); ok {
		segment = append(segment, target)
	}
	flags := recipeFlags(backend, values)
	return append(segment, flags...), nil
}

func recipeExecSegment(values map[string]any) ([]string, error) {
	command, err := stringList(values["command"])
	if err != nil {
		return nil, fmt.Errorf("exec recipe requires command string list")
	}
	flags := recipeFlags("exec", values)
	segment := append([]string{"exec"}, flags...)
	segment = append(segment, "--")
	return append(segment, command...), nil
}

func recipeTarget(backend string, values map[string]any) (string, bool) {
	for _, key := range recipeTargetKeys(backend) {
		if value, ok := values[key]; ok {
			target, scalarOK := scalarString(value)
			return target, scalarOK
		}
	}
	return "", false
}

var recipeBackendTargets = map[string][]string{
	"http":       {"url"},
	"tcp":        {"address", "target"},
	"grpc":       {"address", "target"},
	"ntp":        {"address", "target"},
	"unix":       {"path", "target"},
	"file":       {"path", "target"},
	"log":        {"path", "target"},
	"pidfile":    {"path", "target"},
	"lockfile":   {"path", "target"},
	"permission": {"path", "target"},
	"checksum":   {"path", "target"},
	"archive":    {"path", "target"},
	"k8s":        {"resource", "target"},
	"docker":     {"container", "target"},
	"systemd":    {"unit", "target"},
	"launchd":    {"label", "target"},
	"s3":         {"url", "target"},
	"icmp":       {"host", "pattern", "target"},
	"dns":        {"host", "pattern", "target"},
	"ports":      {"host", "pattern", "target"},
	"ssh":        {"host", "pattern", "target"},
	"tls":        {"host", "pattern", "target"},
	"glob":       {"host", "pattern", "target"},
	"cosign":     {"target", "image", "file", "blob"},
}

func recipeTargetKeys(backend string) []string {
	if keys, ok := recipeBackendTargets[backend]; ok {
		return keys
	}
	return []string{"target"}
}

func recipeFlags(backend string, values map[string]any) []string {
	targets := map[string]bool{}
	for _, key := range recipeTargetKeys(backend) {
		targets[key] = true
	}
	if backend == "exec" {
		targets["command"] = true
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if !targets[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var flags []string
	for _, key := range keys {
		flags = appendRecipeFlag(flags, key, values[key])
	}
	return flags
}

func appendRecipeFlag(flags []string, key string, value any) []string {
	flag := "--" + strings.ReplaceAll(key, "_", "-")
	switch v := value.(type) {
	case bool:
		if v {
			return append(flags, flag)
		}
		return flags
	case []any:
		for _, item := range v {
			if s, ok := scalarString(item); ok {
				flags = append(flags, flag, s)
			}
		}
		return flags
	default:
		if s, ok := scalarString(v); ok {
			return append(flags, flag, s)
		}
		return flags
	}
}

func scalarString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case int, int64, uint64, float64, bool:
		return fmt.Sprint(v), true
	default:
		return "", false
	}
}

func stringValue(value any) (string, bool) {
	s, ok := value.(string)
	return s, ok && strings.TrimSpace(s) != ""
}

func boolValue(value any) bool {
	v, _ := value.(bool)
	return v
}

func stringList(value any) ([]string, error) {
	switch items := value.(type) {
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := scalarString(item)
			if !ok {
				return nil, fmt.Errorf("expected scalar")
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return append([]string(nil), items...), nil
	default:
		return nil, fmt.Errorf("expected list")
	}
}
