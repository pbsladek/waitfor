package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
	"github.com/pbsladek/wait-for/internal/expr"
	"github.com/pbsladek/wait-for/internal/output"
	"github.com/pbsladek/wait-for/internal/runner"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	ExitSatisfied = 0
	ExitTimeout   = 1
	ExitInvalid   = 2
	ExitFatal     = 3
	ExitCancelled = 130
)

type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e exitError) Unwrap() error {
	return e.err
}

func Execute(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	cmd := newCommand(stdin, stdout, stderr)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	if err := cmd.Execute(); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			if ee.err != nil {
				_, _ = fmt.Fprintf(stderr, "waitfor: %v\n", ee.err)
			}
			return ee.code
		}
		_, _ = fmt.Fprintf(stderr, "waitfor: %v\n", err)
		return ExitFatal
	}
	return ExitSatisfied
}

func newCommand(stdin io.Reader, stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:                "waitfor [flags] <backend> <target> [backend-flags] [-- <backend> ...]",
		Short:              "Wait until semantic conditions are satisfied",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if wantsHelp(args) {
				_, _ = io.WriteString(stdout, helpText())
				return nil
			}
			code, err := run(cmd.Context(), args, stdout, stderr)
			if err != nil {
				return exitError{code: code, err: err}
			}
			if code != ExitSatisfied {
				return exitError{code: code}
			}
			return nil
		},
	}
	cmd.SetIn(stdin)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd
}

type globalOptions struct {
	timeout           time.Duration
	interval          time.Duration
	perAttemptTimeout time.Duration
	format            output.Format
	mode              runner.Mode
	verbose           bool
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) (int, error) {
	opts, rest, err := parseGlobal(args)
	if err != nil {
		return ExitInvalid, err
	}
	conditions, err := parseConditions(rest)
	if err != nil {
		return ExitInvalid, err
	}

	outputWriter := stderr
	if opts.format == output.FormatJSON {
		outputWriter = stdout
	}
	printer := output.NewPrinter(outputWriter, opts.format, opts.verbose)
	printer.Start(len(conditions), opts.timeout, opts.interval)
	out, err := runner.Run(ctx, runner.Config{
		Conditions:        conditions,
		Timeout:           opts.timeout,
		Interval:          opts.interval,
		PerAttemptTimeout: opts.perAttemptTimeout,
		Mode:              opts.mode,
		OnAttempt: func(event runner.AttemptEvent) {
			printer.Attempt(output.Attempt{
				Name:      event.Name,
				Attempt:   event.Attempt,
				Satisfied: event.Satisfied,
				Detail:    event.Detail,
				Error:     event.Error,
				Elapsed:   event.Elapsed,
			})
		},
	})
	if err != nil {
		return ExitInvalid, err
	}
	if err := printer.Outcome(reportFromOutcome(out)); err != nil {
		return ExitFatal, err
	}
	switch out.Status {
	case runner.StatusSatisfied:
		return ExitSatisfied, nil
	case runner.StatusFatal:
		return ExitFatal, nil
	case runner.StatusCancelled:
		return ExitCancelled, nil
	default:
		return ExitTimeout, nil
	}
}

func applyFormatAndMode(opts globalOptions, format, mode string) (globalOptions, error) {
	switch output.Format(format) {
	case output.FormatText, output.FormatJSON:
		opts.format = output.Format(format)
	default:
		return opts, fmt.Errorf("invalid output format %q", format)
	}
	switch mode {
	case "all":
		opts.mode = runner.ModeAll
	case "any":
		opts.mode = runner.ModeAny
	default:
		return opts, fmt.Errorf("invalid mode %q", mode)
	}
	if opts.timeout <= 0 {
		return opts, fmt.Errorf("timeout must be positive")
	}
	if opts.interval <= 0 {
		return opts, fmt.Errorf("interval must be positive")
	}
	if opts.perAttemptTimeout < 0 {
		return opts, fmt.Errorf("attempt-timeout cannot be negative")
	}
	return opts, nil
}

func parseGlobal(args []string) (globalOptions, []string, error) {
	opts := globalOptions{
		timeout:  5 * time.Minute,
		interval: 2 * time.Second,
		format:   output.FormatText,
		mode:     runner.ModeAll,
	}

	fs := pflag.NewFlagSet("waitfor", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Stop at the first non-flag argument (the backend name). This means flag
	// values that happen to be backend names are consumed correctly by pflag
	// rather than being misidentified as condition starts.
	fs.SetInterspersed(false)
	var format string
	var mode string
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "global deadline")
	fs.DurationVar(&opts.interval, "interval", opts.interval, "poll interval")
	fs.DurationVar(&opts.perAttemptTimeout, "attempt-timeout", 0, "per-attempt deadline; 0 disables per-attempt limit (global timeout still applies)")
	fs.StringVar(&format, "output", string(opts.format), "output format: text|json")
	fs.StringVar(&mode, "mode", "all", "condition mode: all|any")
	fs.BoolVar(&opts.verbose, "verbose", false, "show every attempt")
	var err error
	if err = fs.Parse(args); err != nil {
		return opts, nil, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return opts, nil, fmt.Errorf("missing condition backend")
	}
	opts, err = applyFormatAndMode(opts, format, mode)
	if err != nil {
		return opts, nil, err
	}
	return opts, rest, nil
}

func parseConditions(args []string) ([]condition.Condition, error) {
	segments, err := splitConditionSegments(args)
	if err != nil {
		return nil, err
	}
	conditions := make([]condition.Condition, 0, len(segments))
	for _, segment := range segments {
		cond, err := parseCondition(segment)
		if err != nil {
			return nil, err
		}
		conditions = append(conditions, cond)
	}
	return conditions, nil
}

func parseCondition(segment []string) (condition.Condition, error) {
	if len(segment) == 0 {
		return nil, fmt.Errorf("empty condition")
	}
	switch segment[0] {
	case "http":
		return parseHTTPCondition(segment)
	case "tcp":
		return parseTCPCondition(segment)
	case "exec":
		return parseExecCondition(segment)
	case "file":
		return parseFileCondition(segment)
	case "k8s":
		return parseKubernetesCondition(segment)
	default:
		return nil, fmt.Errorf("unknown backend %q", segment[0])
	}
}

func validateHTTPURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid http URL %q", rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("http URL must use http or https")
	}
	return nil
}

func parseBodyContent(body, bodyFile string) ([]byte, error) {
	if body != "" && bodyFile != "" {
		return nil, fmt.Errorf("--body and --body-file are mutually exclusive")
	}
	if body != "" {
		return []byte(body), nil
	}
	if bodyFile != "" {
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return nil, fmt.Errorf("read body file: %w", err)
		}
		return data, nil
	}
	return nil, nil
}

func parseHTTPHeaders(rawHeaders []string) (map[string]string, error) {
	headers := make(map[string]string, len(rawHeaders))
	for _, raw := range rawHeaders {
		key, value, ok := splitHeader(raw)
		if !ok {
			return nil, fmt.Errorf("invalid header %q", raw)
		}
		headers[key] = value
	}
	return headers, nil
}

func compileHTTPBodyMatchers(bodyMatches, jsonpath string) (*regexp.Regexp, *expr.Expression, error) {
	var bodyRegex *regexp.Regexp
	if bodyMatches != "" {
		var err error
		bodyRegex, err = regexp.Compile(bodyMatches)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid body regex: %w", err)
		}
	}
	var bodyExpr *expr.Expression
	if jsonpath != "" {
		var err error
		bodyExpr, err = expr.Compile(jsonpath)
		if err != nil {
			return nil, nil, err
		}
	}
	return bodyRegex, bodyExpr, nil
}

func parseHTTPCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("http", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	method := "GET"
	status := "200"
	body := ""
	bodyFile := ""
	bodyContains := ""
	bodyMatches := ""
	jsonpath := ""
	insecure := false
	noRedirects := false
	var rawHeaders []string
	fs.StringVar(&method, "method", method, "HTTP method")
	fs.StringVar(&status, "status", status, "expected HTTP status or class, such as 200 or 2xx")
	fs.StringVar(&body, "body", "", "request body")
	fs.StringVar(&bodyFile, "body-file", "", "request body file")
	fs.StringVar(&bodyContains, "body-contains", bodyContains, "required body substring")
	fs.StringVar(&bodyMatches, "body-matches", bodyMatches, "required body regex")
	fs.StringVar(&jsonpath, "jsonpath", jsonpath, "JSON expression")
	fs.BoolVar(&insecure, "insecure", insecure, "skip TLS verification")
	fs.BoolVar(&noRedirects, "no-follow-redirects", noRedirects, "do not follow HTTP redirects")
	fs.StringArrayVar(&rawHeaders, "header", nil, "request header, as Key: Value or Key=Value")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("http requires exactly one URL")
	}
	if err := validateHTTPURL(args[0]); err != nil {
		return nil, err
	}
	statusMatcher, err := condition.ParseHTTPStatusMatcher(status)
	if err != nil {
		return nil, err
	}
	requestBody, err := parseBodyContent(body, bodyFile)
	if err != nil {
		return nil, err
	}
	bodyRegex, bodyExpr, err := compileHTTPBodyMatchers(bodyMatches, jsonpath)
	if err != nil {
		return nil, err
	}
	headers, err := parseHTTPHeaders(rawHeaders)
	if err != nil {
		return nil, err
	}
	cond := condition.NewHTTP(args[0])
	cond.Method = method
	cond.StatusMatcher = statusMatcher
	cond.RequestBody = requestBody
	cond.BodyContains = bodyContains
	cond.BodyRegex = bodyRegex
	cond.BodyJSONExpr = bodyExpr
	cond.Insecure = insecure
	cond.NoRedirects = noRedirects
	cond.Headers = headers
	return cond, nil
}

func parseTCPCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("tcp", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("tcp requires exactly one host:port address")
	}
	if _, _, err := net.SplitHostPort(args[0]); err != nil {
		return nil, fmt.Errorf("invalid tcp address %q: %w", args[0], err)
	}
	return condition.NewTCP(args[0]), nil
}

func parseFileCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("file", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	contains := ""
	fs.StringVar(&contains, "contains", "", "required file substring")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) < 1 || len(args) > 2 {
		return nil, fmt.Errorf("file requires PATH [exists|deleted|nonempty]")
	}
	state := condition.FileExists
	if len(args) == 2 {
		state = condition.FileState(args[1])
	}
	switch state {
	case condition.FileExists, condition.FileDeleted, condition.FileNonEmpty:
	default:
		return nil, fmt.Errorf("invalid file state %q: must be exists, deleted, or nonempty", state)
	}
	cond := condition.NewFile(args[0], state)
	cond.Contains = contains
	return cond, nil
}

func parseKubernetesCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("k8s", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	namespace := "default"
	conditionName := ""
	jsonpath := ""
	kubeconfig := ""
	fs.StringVar(&namespace, "namespace", namespace, "namespace")
	fs.StringVar(&conditionName, "condition", conditionName, "condition type")
	fs.StringVar(&jsonpath, "jsonpath", jsonpath, "JSON expression")
	fs.StringVar(&kubeconfig, "kubeconfig", kubeconfig, "kubeconfig path")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("k8s requires exactly one RESOURCE (e.g. pod/myapp, deployment/api)")
	}
	if conditionName != "" && jsonpath != "" {
		return nil, fmt.Errorf("--condition and --jsonpath are mutually exclusive")
	}
	var jsonExpr *expr.Expression
	if jsonpath != "" {
		var err error
		jsonExpr, err = expr.Compile(jsonpath)
		if err != nil {
			return nil, err
		}
	}
	cond := condition.NewKubernetes(args[0])
	cond.Namespace = namespace
	cond.Condition = conditionName
	cond.JSONExpr = jsonExpr
	cond.Kubeconfig = kubeconfig
	return cond, nil
}

func validateEnvVars(env []string) error {
	for _, e := range env {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("--env must use KEY=VALUE, got %q", e)
		}
	}
	return nil
}

func parseExecCondition(segment []string) (condition.Condition, error) {
	tokens := segment[1:]
	separator := indexOf(tokens, "--")
	if separator < 0 {
		return nil, fmt.Errorf("exec requires -- before command")
	}
	command := tokens[separator+1:]
	if len(command) == 0 {
		return nil, fmt.Errorf("exec requires a command; use: waitfor exec [flags] -- command [args...]")
	}

	fs := pflag.NewFlagSet("exec", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	exitCode := 0
	outputContains := ""
	jsonpath := ""
	cwd := ""
	var env []string
	maxOutputBytes := int64(0)
	fs.IntVar(&exitCode, "exit-code", exitCode, "expected exit code")
	fs.StringVar(&outputContains, "output-contains", outputContains, "required output substring")
	fs.StringVar(&jsonpath, "jsonpath", jsonpath, "JSON expression")
	fs.StringVar(&cwd, "cwd", cwd, "working directory")
	fs.StringArrayVar(&env, "env", nil, "extra environment variable (KEY=VALUE)")
	fs.Int64Var(&maxOutputBytes, "max-output-bytes", maxOutputBytes, "max output bytes to capture")
	if err := fs.Parse(tokens[:separator]); err != nil {
		return nil, err
	}
	if args := fs.Args(); len(args) != 0 {
		return nil, fmt.Errorf("exec flags must precede --: unexpected args: %s", strings.Join(args, " "))
	}
	if err := validateEnvVars(env); err != nil {
		return nil, err
	}
	var outputExpr *expr.Expression
	if jsonpath != "" {
		var err error
		outputExpr, err = expr.Compile(jsonpath)
		if err != nil {
			return nil, err
		}
	}

	cond := condition.NewExec(command)
	cond.ExpectedExitCode = exitCode
	cond.OutputContains = outputContains
	cond.OutputJSONExpr = outputExpr
	cond.Cwd = cwd
	cond.Env = env
	cond.MaxOutputBytes = maxOutputBytes
	return cond, nil
}

func isSeparatorBefore(args []string, i int) bool {
	return args[i] == "--" && i+1 < len(args) && isBackend(args[i+1])
}

func splitConditionSegments(args []string) ([][]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("missing condition")
	}
	if args[0] == "--" {
		return nil, fmt.Errorf("empty condition before --")
	}
	if args[len(args)-1] == "--" {
		return nil, fmt.Errorf("empty trailing condition")
	}
	var segments [][]string
	var current []string
	for i := 0; i < len(args); i++ {
		if isSeparatorBefore(args, i) {
			if len(current) == 0 {
				return nil, fmt.Errorf("empty condition before --")
			}
			segments = append(segments, current)
			current = nil
			continue
		}
		current = append(current, args[i])
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("empty trailing condition")
	}
	segments = append(segments, current)
	return segments, nil
}

func isBackend(arg string) bool {
	switch arg {
	case "http", "tcp", "exec", "file", "k8s":
		return true
	default:
		return false
	}
}

func wantsHelp(args []string) bool {
	if len(args) == 0 {
		return true
	}
	return args[0] == "-h" || args[0] == "--help" || args[0] == "help"
}

func reportFromOutcome(out runner.Outcome) output.Report {
	report := output.Report{
		Status:          string(out.Status),
		Satisfied:       out.Satisfied(),
		Mode:            string(out.Mode),
		ElapsedSeconds:  output.Seconds(out.Elapsed),
		TimeoutSeconds:  output.Seconds(out.Timeout),
		IntervalSeconds: output.Seconds(out.Interval),
		Conditions:      make([]output.ConditionReport, 0, len(out.Conditions)),
	}
	if out.PerAttemptTimeout > 0 {
		report.PerAttemptTimeoutSeconds = output.Seconds(out.PerAttemptTimeout)
	}
	for _, rec := range out.Conditions {
		report.Conditions = append(report.Conditions, output.ConditionReport{
			Backend:        rec.Backend,
			Target:         rec.Target,
			Name:           rec.Name,
			Satisfied:      rec.Satisfied,
			Attempts:       rec.Attempts,
			ElapsedSeconds: output.Seconds(rec.Elapsed),
			Detail:         rec.Detail,
			LastError:      rec.LastError,
			Fatal:          rec.Fatal,
		})
	}
	return report
}

func splitHeader(raw string) (string, string, bool) {
	if key, value, ok := strings.Cut(raw, ":"); ok {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		return key, value, key != ""
	}
	if key, value, ok := strings.Cut(raw, "="); ok {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		return key, value, key != ""
	}
	return "", "", false
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

func helpText() string {
	return `waitfor - semantic condition poller

Usage:
  waitfor [flags] <backend> <target> [backend-flags]
  waitfor [flags] <backend> ... -- <backend> ...

Global flags:
  --timeout duration       Global deadline (default: 5m)
  --interval duration      Poll interval (default: 2s)
  --attempt-timeout duration
                           Per-attempt deadline; 0 disables (default: 0)
  --output text|json       Output format (default: text); JSON goes to stdout
  --mode all|any           Condition mode (default: all)
  --verbose                Show each poll attempt

HTTP:
  waitfor http [flags] URL
  --status 200|2xx         Expected status code or class (default: 200)
  --method GET             HTTP method (default: GET)
  --body text              Request body string
  --body-file path         Request body from file (mutually exclusive with --body)
  --body-contains text     Required response body substring
  --body-matches regex     Required response body regex
  --jsonpath expr          Required JSON expression on response body
  --header Key=Value       Request header (repeatable; Key: Value also accepted)
  --insecure               Skip TLS certificate verification
  --no-follow-redirects    Do not follow HTTP redirects

TCP:
  waitfor tcp HOST:PORT

Exec:
  waitfor exec [flags] -- COMMAND [ARGS...]
  --exit-code 0            Expected exit code (default: 0)
  --output-contains text   Required stdout/stderr substring
  --jsonpath expr          Required JSON expression on stdout
  --cwd path               Working directory for the command
  --env KEY=VALUE          Extra environment variable (repeatable)
  --max-output-bytes N     Capture at most N bytes of output (default: unlimited)

File:
  waitfor file PATH [exists|deleted|nonempty]
  --contains text          Required file content substring (only with exists/nonempty)

Kubernetes:
  waitfor k8s [flags] RESOURCE
  RESOURCE format: kind/name  (e.g. pod/myapp, deployment/api, job/migrate)
  --condition type         Condition type to check (default: Ready)
  --jsonpath expr          JSON expression on the resource (mutually exclusive with --condition)
  --namespace ns           Namespace (default: default)
  --kubeconfig path        Path to kubeconfig file

Examples:
  waitfor http https://api.example.com/health --status 200
  waitfor tcp localhost:5432
  waitfor file /tmp/ready.flag exists
  waitfor exec --output-contains Running -- kubectl get pod myapp
  waitfor k8s deployment/api --condition Available
  waitfor --timeout 10m http https://api.example.com/health -- tcp localhost:5432

Exit codes:
  0    all (or any) conditions satisfied
  1    timeout expired
  2    invalid arguments or configuration
  3    unrecoverable condition failure (e.g. command not found, bad k8s config)
  130  cancelled by SIGINT or parent context cancellation
  143  cancelled by SIGTERM
`
}
