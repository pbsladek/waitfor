package cli

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
	"github.com/pbsladek/wait-for/internal/expr"
	"github.com/pbsladek/wait-for/internal/output"
	"github.com/pbsladek/wait-for/internal/runner"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	ExitSatisfied = 0
	ExitTimeout   = 1
	ExitInvalid   = 2
	ExitFatal     = 3
	ExitCancelled = 130

	maxHTTPBodyFileBytes = 10 * 1024 * 1024
	maxTLSCAFileBytes    = 10 * 1024 * 1024
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
			if isDoctorCommand(args) {
				code, err := runDoctor(cmd.Context(), args[1:], stdout, stderr)
				if err != nil {
					return exitError{code: code, err: err}
				}
				if code != ExitSatisfied {
					return exitError{code: code}
				}
				return nil
			}
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
	maxInterval       time.Duration
	backoff           runner.Backoff
	jitter            float64
	perAttemptTimeout time.Duration
	requiredSuccesses int
	stableFor         time.Duration
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

	runCfg := runner.Config{
		Conditions:        conditions,
		Timeout:           opts.timeout,
		Interval:          opts.interval,
		MaxInterval:       opts.maxInterval,
		Backoff:           opts.backoff,
		Jitter:            opts.jitter,
		PerAttemptTimeout: opts.perAttemptTimeout,
		RequiredSuccesses: opts.requiredSuccesses,
		StableFor:         opts.stableFor,
		Mode:              opts.mode,
	}
	if err := runner.ValidateConfig(runCfg); err != nil {
		return ExitInvalid, err
	}

	outputWriter := stderr
	if opts.format == output.FormatJSON {
		outputWriter = stdout
	}
	printer := output.NewPrinter(outputWriter, opts.format, opts.verbose)
	printer.Start(len(conditions), opts.timeout, opts.interval)
	runCfg.OnAttempt = func(event runner.AttemptEvent) {
		printer.Attempt(output.Attempt{
			Name:      event.Name,
			Attempt:   event.Attempt,
			Satisfied: event.Satisfied,
			Detail:    event.Detail,
			Error:     event.Error,
			Elapsed:   event.Elapsed,
		})
	}
	out, err := runner.Run(ctx, runCfg)
	if err != nil {
		return ExitInvalid, err
	}
	if err := printer.Outcome(reportFromOutcome(out)); err != nil {
		return ExitFatal, err
	}
	return exitCodeForStatus(out.Status), nil
}

func exitCodeForStatus(status runner.Status) int {
	switch status {
	case runner.StatusSatisfied:
		return ExitSatisfied
	case runner.StatusFatal:
		return ExitFatal
	case runner.StatusCancelled:
		return ExitCancelled
	default:
		return ExitTimeout
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
	if err := validateGeneralOptions(opts); err != nil {
		return opts, err
	}
	return opts, nil
}

func validateGeneralOptions(opts globalOptions) error {
	if opts.timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	if opts.interval <= 0 {
		return fmt.Errorf("interval must be positive")
	}
	if err := validateBackoffOptions(opts); err != nil {
		return err
	}
	if opts.perAttemptTimeout < 0 {
		return fmt.Errorf("attempt-timeout cannot be negative")
	}
	if opts.requiredSuccesses < 1 {
		return fmt.Errorf("successes must be at least 1")
	}
	if opts.stableFor < 0 {
		return fmt.Errorf("stable-for cannot be negative")
	}
	return nil
}

func validateBackoffOptions(opts globalOptions) error {
	if opts.maxInterval < opts.interval {
		return fmt.Errorf("max-interval must be greater than or equal to interval")
	}
	if opts.backoff != runner.BackoffConstant && opts.backoff != runner.BackoffExponential {
		return fmt.Errorf("invalid backoff %q", opts.backoff)
	}
	if math.IsNaN(opts.jitter) || math.IsInf(opts.jitter, 0) || opts.jitter < 0 || opts.jitter > 1 {
		return fmt.Errorf("jitter must be between 0 and 100%%")
	}
	return nil
}

func parseGlobal(args []string) (globalOptions, []string, error) {
	opts := globalOptions{
		timeout:           5 * time.Minute,
		interval:          2 * time.Second,
		backoff:           runner.BackoffConstant,
		requiredSuccesses: 1,
		format:            output.FormatText,
		mode:              runner.ModeAll,
	}

	fs := pflag.NewFlagSet("waitfor", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Stop at the first non-flag argument (the backend name). This means flag
	// values that happen to be backend names are consumed correctly by pflag
	// rather than being misidentified as condition starts.
	fs.SetInterspersed(false)
	var format string
	var mode string
	var backoff string
	var jitter string
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "global deadline")
	fs.DurationVar(&opts.interval, "interval", opts.interval, "poll interval")
	fs.DurationVar(&opts.maxInterval, "max-interval", 0, "maximum poll interval for exponential backoff; defaults to --interval")
	fs.StringVar(&backoff, "backoff", string(opts.backoff), "poll backoff strategy: constant|exponential")
	fs.StringVar(&jitter, "jitter", "0%", "poll interval jitter, as a percentage such as 20% or a fraction such as 0.2")
	fs.DurationVar(&opts.perAttemptTimeout, "attempt-timeout", 0, "per-attempt deadline; 0 disables per-attempt limit (global timeout still applies)")
	fs.IntVar(&opts.requiredSuccesses, "successes", opts.requiredSuccesses, "consecutive successful checks required before a condition is satisfied")
	fs.DurationVar(&opts.stableFor, "stable-for", 0, "duration a condition must remain continuously successful before it is satisfied")
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
	opts.backoff = runner.Backoff(strings.ToLower(backoff))
	if opts.maxInterval == 0 {
		opts.maxInterval = opts.interval
	}
	opts.jitter, err = parseJitter(jitter)
	if err != nil {
		return opts, nil, err
	}
	opts, err = applyFormatAndMode(opts, format, mode)
	if err != nil {
		return opts, nil, err
	}
	return opts, rest, nil
}

func parseJitter(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("jitter is required")
	}
	if strings.HasSuffix(raw, "%") {
		value, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid jitter %q", raw)
		}
		return value / 100, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid jitter %q", raw)
	}
	return value, nil
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

type backendParser func([]string) (condition.Condition, error)

var backendParsers = map[string]backendParser{
	"http":       parseHTTPCondition,
	"tcp":        parseTCPCondition,
	"unix":       parseUnixCondition,
	"tls":        parseTLSCondition,
	"ssh":        parseSSHCondition,
	"s3":         parseS3Condition,
	"glob":       parseGlobCondition,
	"ports":      parsePortsCondition,
	"exec":       parseExecCondition,
	"file":       parseFileCondition,
	"log":        parseLogCondition,
	"k8s":        parseKubernetesCondition,
	"dns":        parseDNSCondition,
	"docker":     parseDockerCondition,
	"process":    parseProcessCondition,
	"systemd":    parseSystemdCondition,
	"launchd":    parseLaunchdCondition,
	"pidfile":    parsePIDFileCondition,
	"lockfile":   parseLockfileCondition,
	"permission": parsePermissionCondition,
	"checksum":   parseChecksumCondition,
	"archive":    parseArchiveCondition,
	"cosign":     parseCosignCondition,
	"ntp":        parseNTPCondition,
	"icmp":       parseICMPCondition,
	"grpc":       parseGRPCCondition,
	"websocket":  parseWebSocketCondition,
}

func parseCondition(segment []string) (condition.Condition, error) {
	if len(segment) == 0 {
		return nil, fmt.Errorf("empty condition")
	}
	if segment[0] == "guard" {
		if len(segment) == 1 {
			return nil, fmt.Errorf("guard requires a backend condition")
		}
		inner, err := parseCondition(segment[1:])
		if err != nil {
			return nil, err
		}
		return condition.NewGuard(inner), nil
	}
	segment, name, err := parseConditionName(segment)
	if err != nil {
		return nil, err
	}
	parser, ok := backendParsers[segment[0]]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", segment[0])
	}
	cond, err := parser(segment)
	if err != nil {
		return nil, err
	}
	if name != "" {
		cond = condition.WithName(cond, name)
	}
	return cond, nil
}

func parseConditionName(segment []string) ([]string, string, error) {
	if conditionNameReservedForBackend(segment) {
		return segment, "", nil
	}
	cleaned := []string{segment[0]}
	name := ""
	limit := conditionFlagLimit(segment)
	for i := 1; i < len(segment); i++ {
		if i >= limit {
			cleaned = append(cleaned, segment[i:]...)
			break
		}
		arg := segment[i]
		if arg == "--name" {
			if i+1 >= limit {
				return nil, "", fmt.Errorf("--name requires a value")
			}
			var err error
			name, err = setConditionName(name, segment[i+1])
			if err != nil {
				return nil, "", err
			}
			i++
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--name="); ok {
			var err error
			name, err = setConditionName(name, value)
			if err != nil {
				return nil, "", err
			}
			continue
		}
		cleaned = append(cleaned, arg)
	}
	return cleaned, name, nil
}

func conditionNameReservedForBackend(segment []string) bool {
	return len(segment) > 0 && segment[0] == "process"
}

func conditionFlagLimit(segment []string) int {
	if len(segment) == 0 || segment[0] != "exec" {
		return len(segment)
	}
	if separator := indexOf(segment[1:], "--"); separator >= 0 {
		return separator + 1
	}
	return len(segment)
}

func setConditionName(current, next string) (string, error) {
	next = strings.TrimSpace(next)
	if next == "" {
		return "", fmt.Errorf("--name cannot be empty")
	}
	if current != "" {
		return "", fmt.Errorf("--name can be set only once per condition")
	}
	return next, nil
}

func validateHTTPURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid http URL")
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
		data, err := readFileLimit(bodyFile, maxHTTPBodyFileBytes)
		if err != nil {
			return nil, fmt.Errorf("read body file: %w", err)
		}
		return data, nil
	}
	return nil, nil
}

func parseHTTPHeaders(rawHeaders []string) (map[string]string, error) {
	headers := make(map[string]string, len(rawHeaders))
	seen := make(map[string]bool, len(rawHeaders))
	for _, raw := range rawHeaders {
		key, value, ok := splitHeader(raw)
		if !ok {
			return nil, fmt.Errorf("invalid header; must use Key: Value or Key=Value")
		}
		if !validHTTPHeaderName(key) {
			return nil, fmt.Errorf("invalid HTTP header name %q", key)
		}
		if !validHTTPHeaderValue(value) {
			return nil, fmt.Errorf("invalid HTTP header value for %q", key)
		}
		canonical := strings.ToLower(key)
		if seen[canonical] {
			return nil, fmt.Errorf("duplicate HTTP header %q", key)
		}
		seen[canonical] = true
		headers[key] = value
	}
	return headers, nil
}

func validHTTPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isHTTPTokenChar(name[i]) {
			return false
		}
	}
	return true
}

func isHTTPTokenChar(ch byte) bool {
	if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' {
		return true
	}
	switch ch {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

func validHTTPHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch == '\t' {
			continue
		}
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func compileHTTPBodyMatchers(bodyMatches, jsonpath string) (*regexp.Regexp, *expr.Expression, error) {
	var bodyRegex *regexp.Regexp
	if bodyMatches != "" {
		var err error
		bodyRegex, err = regexp.Compile(bodyMatches)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid body regex")
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

func parseUnixCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("unix", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("unix requires exactly one socket PATH")
	}
	if strings.TrimSpace(args[0]) == "" {
		return nil, fmt.Errorf("unix socket path is required")
	}
	return condition.NewUnix(args[0]), nil
}

func parseTLSCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("tls", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serverName := ""
	validForRaw := ""
	caFile := ""
	fs.StringVar(&serverName, "servername", serverName, "TLS server name for SNI and SAN verification")
	fs.StringVar(&validForRaw, "valid-for", validForRaw, "minimum remaining certificate validity, such as 720h or 30d")
	fs.StringVar(&caFile, "ca-file", caFile, "PEM CA bundle to trust in addition to system roots")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("tls requires exactly one HOST:PORT address")
	}
	if _, _, err := net.SplitHostPort(args[0]); err != nil {
		return nil, fmt.Errorf("invalid tls address %q: %w", args[0], err)
	}
	validFor, err := parseTLSValidFor(validForRaw)
	if err != nil {
		return nil, err
	}
	rootCAs, err := parseTLSRootCAs(caFile)
	if err != nil {
		return nil, err
	}
	cond := condition.NewTLS(args[0])
	cond.ServerName = serverName
	cond.ValidFor = validFor
	cond.RootCAs = rootCAs
	return cond, nil
}

func parseTLSValidFor(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	if strings.HasSuffix(raw, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(raw, "d"), 10, 64)
		maxDays := int64(math.MaxInt64) / int64(24*time.Hour)
		if err != nil || days < 0 || days > maxDays {
			return 0, fmt.Errorf("invalid --valid-for duration %q", raw)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration < 0 {
		return 0, fmt.Errorf("invalid --valid-for duration %q", raw)
	}
	return duration, nil
}

func parseTLSRootCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	data, err := readFileLimit(path, maxTLSCAFileBytes)
	if err != nil {
		return nil, fmt.Errorf("read ca file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("ca file contains no PEM certificates")
	}
	return pool, nil
}

func parseSSHCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("ssh", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bannerContains := ""
	user := ""
	password := ""
	hostKeySHA256 := ""
	fs.StringVar(&bannerContains, "banner-contains", bannerContains, "required SSH banner substring")
	fs.StringVar(&user, "user", user, "SSH username for password auth handshake")
	fs.StringVar(&password, "password", password, "SSH password for auth handshake")
	fs.StringVar(&hostKeySHA256, "host-key-sha256", hostKeySHA256, "required SSH host key SHA256 fingerprint")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("ssh requires exactly one HOST:PORT address")
	}
	if _, _, err := net.SplitHostPort(args[0]); err != nil {
		return nil, fmt.Errorf("invalid ssh address %q: %w", args[0], err)
	}
	if (user == "") != (password == "") {
		return nil, fmt.Errorf("--user and --password must be provided together")
	}
	if user != "" && hostKeySHA256 == "" {
		return nil, fmt.Errorf("--host-key-sha256 is required with password auth")
	}
	cond := condition.NewSSH(args[0])
	cond.BannerContains = bannerContains
	cond.User = user
	cond.Password = password
	cond.HostKeySHA256 = hostKeySHA256
	return cond, nil
}

func parseS3Condition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("s3", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	endpointURL := defaultS3EndpointURL()
	region := defaultS3Region()
	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	sessionToken := os.Getenv("AWS_SESSION_TOKEN")
	contains := ""
	virtualHostedStyle := false
	exists := false
	var rawMetadata []string
	fs.BoolVar(&exists, "exists", exists, "wait until the bucket or object exists")
	fs.StringArrayVar(&rawMetadata, "metadata", nil, "required object metadata as Key=Value; repeatable")
	fs.StringVar(&contains, "contains", contains, "required object content substring")
	fs.StringVar(&endpointURL, "endpoint-url", endpointURL, "S3-compatible endpoint URL; defaults to AWS_ENDPOINT_URL_S3, AWS_ENDPOINT_URL, or S3_ENDPOINT_URL")
	fs.StringVar(&region, "region", region, "AWS region or S3-compatible signing region")
	fs.BoolVar(&virtualHostedStyle, "virtual-hosted-style", virtualHostedStyle, "use bucket.endpoint host style with --endpoint-url")
	fs.StringVar(&accessKeyID, "access-key-id", accessKeyID, "AWS access key id; defaults to AWS_ACCESS_KEY_ID")
	fs.StringVar(&secretAccessKey, "secret-access-key", secretAccessKey, "AWS secret access key; defaults to AWS_SECRET_ACCESS_KEY")
	fs.StringVar(&sessionToken, "session-token", sessionToken, "AWS session token; defaults to AWS_SESSION_TOKEN")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("s3 requires exactly one s3://bucket[/key] URL")
	}
	target, err := condition.ParseS3URL(args[0])
	if err != nil {
		return nil, err
	}
	metadata, err := parseS3Metadata(rawMetadata)
	if err != nil {
		return nil, err
	}
	creds := condition.S3Credentials{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		SessionToken:    sessionToken,
	}
	if err := validateS3CLIOptions(target, endpointURL, region, contains, metadata, creds); err != nil {
		return nil, err
	}
	cond := condition.NewS3(args[0])
	cond.EndpointURL = endpointURL
	cond.Region = region
	cond.VirtualHostedStyle = virtualHostedStyle
	cond.Metadata = metadata
	cond.Contains = contains
	cond.Credentials = creds
	return cond, nil
}

func validateS3CLIOptions(target condition.S3Target, endpointURL, region, contains string, metadata map[string]string, creds condition.S3Credentials) error {
	if err := validateS3CLITargetOptions(target, region, contains, metadata); err != nil {
		return err
	}
	if err := validateS3EndpointURL(endpointURL); err != nil {
		return err
	}
	return validateS3CLICredentialTransport(endpointURL, creds)
}

func validateS3CLITargetOptions(target condition.S3Target, region, contains string, metadata map[string]string) error {
	if contains != "" && target.Key == "" {
		return fmt.Errorf("--contains requires an object key")
	}
	if len(metadata) > 0 && target.Key == "" {
		return fmt.Errorf("--metadata requires an object key")
	}
	if strings.TrimSpace(region) == "" {
		return fmt.Errorf("--region cannot be empty")
	}
	return nil
}

func validateS3CLICredentialTransport(endpointURL string, creds condition.S3Credentials) error {
	if endpointURL == "" || creds.AccessKeyID == "" {
		return nil
	}
	parsed, _ := url.Parse(endpointURL)
	if parsed.Scheme == "http" {
		return fmt.Errorf("s3 credentials require an https endpoint")
	}
	return nil
}

func validateS3EndpointURL(endpointURL string) error {
	if endpointURL == "" {
		return nil
	}
	parsed, err := url.Parse(endpointURL)
	if err != nil {
		return fmt.Errorf("invalid s3 endpoint URL")
	}
	return validateParsedS3EndpointURL(parsed)
}

func validateParsedS3EndpointURL(parsed *url.URL) error {
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid s3 endpoint URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("s3 endpoint URL must use http or https")
	}
	if parsed.User != nil {
		return fmt.Errorf("s3 endpoint URL cannot include userinfo")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("s3 endpoint URL cannot include query or fragment")
	}
	return nil
}

func defaultS3Region() string {
	if region := os.Getenv("AWS_REGION"); region != "" {
		return region
	}
	if region := os.Getenv("AWS_DEFAULT_REGION"); region != "" {
		return region
	}
	return "us-east-1"
}

func defaultS3EndpointURL() string {
	for _, name := range []string{"AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL", "S3_ENDPOINT_URL"} {
		if endpointURL := os.Getenv(name); endpointURL != "" {
			return endpointURL
		}
	}
	return ""
}

func parseS3Metadata(rawValues []string) (map[string]string, error) {
	metadata := make(map[string]string, len(rawValues))
	for _, raw := range rawValues {
		key, value, ok := strings.Cut(raw, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid s3 metadata; must use Key=Value")
		}
		metadata[key] = value
	}
	return metadata, nil
}

func parseGlobCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("glob", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	minCount := 1
	maxCount := -1
	absent := false
	fs.IntVar(&minCount, "min-count", minCount, "minimum number of matches")
	fs.IntVar(&maxCount, "max-count", maxCount, "maximum number of matches; -1 disables")
	fs.BoolVar(&absent, "absent", absent, "wait until no files match")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("glob requires exactly one PATTERN")
	}
	if absent && fs.Changed("min-count") && minCount != 0 {
		return nil, fmt.Errorf("--absent cannot be combined with positive --min-count")
	}
	if absent && !fs.Changed("min-count") {
		minCount = 0
	}
	if err := validateGlobCLIOptions(minCount, maxCount); err != nil {
		return nil, err
	}
	cond := condition.NewGlob(args[0])
	cond.MinCount = minCount
	cond.MaxCount = maxCount
	cond.Absent = absent
	return cond, nil
}

func validateGlobCLIOptions(minCount, maxCount int) error {
	if minCount < 0 {
		return fmt.Errorf("--min-count cannot be negative")
	}
	if maxCount < -1 {
		return fmt.Errorf("--max-count cannot be less than -1")
	}
	if maxCount >= 0 && minCount > maxCount {
		return fmt.Errorf("--min-count cannot exceed --max-count")
	}
	return nil
}

func parsePortsCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("ports", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rangeRaw := ""
	anyMode := false
	allMode := false
	fs.StringVar(&rangeRaw, "range", rangeRaw, "port range START-END")
	fs.BoolVar(&anyMode, "any", anyMode, "succeed when any port in the range is open")
	fs.BoolVar(&allMode, "all", allMode, "succeed when all ports in the range are open")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("ports requires exactly one HOST")
	}
	startPort, endPort, err := parsePortRange(rangeRaw)
	if err != nil {
		return nil, err
	}
	mode, err := parsePortsMode(anyMode, allMode)
	if err != nil {
		return nil, err
	}
	cond := condition.NewPorts(args[0], startPort, endPort)
	cond.Mode = mode
	return cond, nil
}

func parsePortRange(raw string) (int, int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, 0, fmt.Errorf("ports requires --range START-END")
	}
	startRaw, endRaw, ok := strings.Cut(raw, "-")
	if !ok {
		return 0, 0, fmt.Errorf("invalid ports range %q", raw)
	}
	startPort, err := strconv.Atoi(strings.TrimSpace(startRaw))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid ports range %q", raw)
	}
	endPort, err := strconv.Atoi(strings.TrimSpace(endRaw))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid ports range %q", raw)
	}
	if !validPortRange(startPort, endPort) {
		return 0, 0, fmt.Errorf("invalid ports range %q", raw)
	}
	return startPort, endPort, nil
}

func validPortRange(startPort, endPort int) bool {
	return startPort >= 1 && endPort <= 65535 && startPort <= endPort
}

func parsePortsMode(anyMode, allMode bool) (condition.PortsMode, error) {
	if anyMode && allMode {
		return "", fmt.Errorf("--any and --all are mutually exclusive")
	}
	if anyMode {
		return condition.PortsAny, nil
	}
	return condition.PortsAll, nil
}

type dnsParseOptions struct {
	recordType   string
	resolverMode string
	contains     string
	equals       []string
	minCount     int
	absent       bool
	absentMode   string
	server       string
	rcode        string
	transport    string
	edns0        bool
	udpSize      int
}

func parseDNSCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("dns", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := dnsParseOptions{
		recordType:   string(condition.DNSRecordA),
		resolverMode: string(condition.DNSResolverSystem),
		absentMode:   string(condition.DNSAbsentAny),
		transport:    string(condition.DNSTransportUDP),
	}
	fs.StringVar(&opts.recordType, "type", opts.recordType, "DNS record type: A|AAAA|CNAME|TXT|ANY|MX|SRV|NS|CAA|HTTPS|SVCB")
	fs.StringVar(&opts.resolverMode, "resolver", opts.resolverMode, "resolver mode: system|wire")
	fs.StringVar(&opts.contains, "contains", opts.contains, "required record substring")
	fs.StringArrayVar(&opts.equals, "equals", nil, "required exact record value; repeatable")
	fs.IntVar(&opts.minCount, "min-count", opts.minCount, "minimum answer count")
	fs.BoolVar(&opts.absent, "absent", opts.absent, "wait until the record is absent")
	fs.StringVar(&opts.absentMode, "absent-mode", opts.absentMode, "wire absence mode: any|nxdomain|nodata")
	fs.StringVar(&opts.server, "server", opts.server, "DNS server address; port defaults to 53")
	fs.StringVar(&opts.rcode, "rcode", opts.rcode, "wire response code, such as NOERROR or NXDOMAIN")
	fs.StringVar(&opts.transport, "transport", opts.transport, "wire transport: udp|tcp")
	fs.BoolVar(&opts.edns0, "edns0", opts.edns0, "enable EDNS0 for wire resolver")
	fs.IntVar(&opts.udpSize, "udp-size", opts.udpSize, "wire EDNS0 UDP payload size")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("dns requires exactly one HOST")
	}
	if err := condition.ValidateDNSName(args[0]); err != nil {
		return nil, fmt.Errorf("invalid dns name: %w", err)
	}
	opts, serverAddr, err := normalizeDNSOptions(opts)
	if err != nil {
		return nil, err
	}
	udpSize, err := checkedDNSUDPSize(opts.udpSize)
	if err != nil {
		return nil, err
	}
	cond := condition.NewDNS(args[0])
	cond.RecordType = condition.DNSRecordType(opts.recordType)
	cond.ResolverMode = condition.DNSResolverMode(opts.resolverMode)
	cond.Contains = opts.contains
	cond.Equals = opts.equals
	cond.MinCount = opts.minCount
	cond.Absent = opts.absent
	cond.AbsentMode = condition.DNSAbsentMode(opts.absentMode)
	cond.Server = serverAddr
	cond.RCode = strings.ToUpper(opts.rcode)
	cond.Transport = condition.DNSTransport(opts.transport)
	cond.EDNS0 = opts.edns0
	cond.UDPSize = udpSize
	return cond, nil
}

func checkedDNSUDPSize(size int) (uint16, error) {
	if size < 0 || size > 65535 {
		return 0, fmt.Errorf("udp-size must be between 0 and 65535")
	}
	return uint16(size), nil // #nosec G115 -- range is checked immediately above.
}

func normalizeDNSOptions(opts dnsParseOptions) (dnsParseOptions, string, error) {
	opts.recordType = strings.ToUpper(opts.recordType)
	if !validDNSRecordType(condition.DNSRecordType(opts.recordType)) {
		return opts, "", fmt.Errorf("invalid dns record type %q", opts.recordType)
	}
	opts.resolverMode = strings.ToLower(opts.resolverMode)
	if err := validateDNSResolverMode(opts.resolverMode, condition.DNSRecordType(opts.recordType)); err != nil {
		return opts, "", err
	}
	if err := validateDNSMatchers(opts); err != nil {
		return opts, "", err
	}
	opts.absentMode = strings.ToLower(opts.absentMode)
	opts.transport = strings.ToLower(opts.transport)
	if err := validateDNSWireOptions(opts); err != nil {
		return opts, "", err
	}
	serverAddr, err := condition.NormalizeDNSServer(opts.server)
	if err != nil {
		return opts, "", err
	}
	if opts.resolverMode == string(condition.DNSResolverWire) && serverAddr == "" {
		return opts, "", fmt.Errorf("--resolver wire requires --server")
	}
	return opts, serverAddr, nil
}

func validDNSRecordType(recordType condition.DNSRecordType) bool {
	return dnsCLIRecordTypes[recordType]
}

var dnsCLIRecordTypes = map[condition.DNSRecordType]bool{
	condition.DNSRecordA:     true,
	condition.DNSRecordAAAA:  true,
	condition.DNSRecordCNAME: true,
	condition.DNSRecordTXT:   true,
	condition.DNSRecordANY:   true,
	condition.DNSRecordMX:    true,
	condition.DNSRecordSRV:   true,
	condition.DNSRecordNS:    true,
	condition.DNSRecordCAA:   true,
	condition.DNSRecordHTTPS: true,
	condition.DNSRecordSVCB:  true,
}

func validateDNSResolverMode(resolverMode string, recordType condition.DNSRecordType) error {
	if resolverMode != string(condition.DNSResolverSystem) && resolverMode != string(condition.DNSResolverWire) {
		return fmt.Errorf("invalid dns resolver %q", resolverMode)
	}
	if resolverMode == string(condition.DNSResolverSystem) && !systemDNSRecordType(recordType) {
		return fmt.Errorf("dns record type %s requires --resolver wire", recordType)
	}
	return nil
}

func validateDNSMatchers(opts dnsParseOptions) error {
	if opts.minCount < 0 {
		return fmt.Errorf("min-count cannot be negative")
	}
	if opts.absent && (opts.contains != "" || len(opts.equals) > 0 || opts.minCount > 0) {
		return fmt.Errorf("--absent cannot be combined with --contains, --equals, or --min-count")
	}
	return nil
}

func validateDNSWireOptions(opts dnsParseOptions) error {
	if err := validateDNSAbsentOptions(opts); err != nil {
		return err
	}
	if err := validateDNSTransportOptions(opts); err != nil {
		return err
	}
	if opts.udpSize < 0 || opts.udpSize > 65535 {
		return fmt.Errorf("udp-size must be between 0 and 65535")
	}
	return nil
}

func validateDNSAbsentOptions(opts dnsParseOptions) error {
	if opts.absentMode != "any" && opts.absentMode != "nxdomain" && opts.absentMode != "nodata" {
		return fmt.Errorf("invalid dns absent-mode %q", opts.absentMode)
	}
	if opts.absentMode != "any" && opts.resolverMode != string(condition.DNSResolverWire) {
		return fmt.Errorf("--absent-mode requires --resolver wire")
	}
	return nil
}

func validateDNSTransportOptions(opts dnsParseOptions) error {
	if opts.transport != "udp" && opts.transport != "tcp" {
		return fmt.Errorf("invalid dns transport %q", opts.transport)
	}
	if opts.rcode != "" && !condition.ValidDNSRCode(opts.rcode) {
		return fmt.Errorf("invalid dns rcode %q", strings.ToUpper(opts.rcode))
	}
	if usesWireOnlyOptions(opts) && opts.resolverMode != string(condition.DNSResolverWire) {
		return fmt.Errorf("--rcode, --transport, --edns0, and --udp-size require --resolver wire")
	}
	return nil
}

func usesWireOnlyOptions(opts dnsParseOptions) bool {
	return opts.rcode != "" || opts.transport == "tcp" || opts.edns0 || opts.udpSize > 0
}

func systemDNSRecordType(recordType condition.DNSRecordType) bool {
	switch recordType {
	case condition.DNSRecordA, condition.DNSRecordAAAA, condition.DNSRecordCNAME, condition.DNSRecordTXT, condition.DNSRecordANY:
		return true
	default:
		return false
	}
}

func parseDockerCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	status := "running"
	health := ""
	fs.StringVar(&status, "status", status, "container status: any|created|running|paused|restarting|removing|exited|dead")
	fs.StringVar(&health, "health", health, "container health: healthy|unhealthy|starting|none")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("docker requires exactly one CONTAINER")
	}
	status = strings.ToLower(status)
	if !validDockerStatus(status) {
		return nil, fmt.Errorf("invalid docker status %q", status)
	}
	health = strings.ToLower(health)
	if !validDockerHealth(health) {
		return nil, fmt.Errorf("invalid docker health %q", health)
	}
	cond := condition.NewDocker(args[0])
	cond.Status = status
	cond.Health = health
	return cond, nil
}

func parseProcessCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("process", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pid := 0
	name := ""
	running := false
	stopped := false
	fs.IntVar(&pid, "pid", pid, "process ID")
	fs.StringVar(&name, "name", name, "process executable name")
	fs.BoolVar(&running, "running", running, "wait until the process is running")
	fs.BoolVar(&stopped, "stopped", stopped, "wait until the process is stopped")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 0 {
		return nil, fmt.Errorf("process does not accept positional arguments; use --pid or --name")
	}
	state, err := parseProcessState(running, stopped)
	if err != nil {
		return nil, err
	}
	if err := validateProcessSelector(pid, name); err != nil {
		return nil, err
	}
	cond := condition.NewProcess()
	cond.PID = pid
	cond.Name = name
	cond.State = state
	return cond, nil
}

func parseProcessState(running, stopped bool) (condition.ProcessState, error) {
	if running && stopped {
		return "", fmt.Errorf("--running and --stopped are mutually exclusive")
	}
	if stopped {
		return condition.ProcessStopped, nil
	}
	return condition.ProcessRunning, nil
}

func validateProcessSelector(pid int, name string) error {
	if pid < 0 {
		return fmt.Errorf("--pid must be positive")
	}
	if pid == 0 && strings.TrimSpace(name) == "" {
		return fmt.Errorf("process requires exactly one of --pid or --name")
	}
	if pid > 0 && strings.TrimSpace(name) != "" {
		return fmt.Errorf("--pid and --name are mutually exclusive")
	}
	return nil
}

func parseSystemdCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("systemd", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	active := false
	inactive := false
	failed := false
	fs.BoolVar(&active, "active", active, "wait until the unit is active")
	fs.BoolVar(&inactive, "inactive", inactive, "wait until the unit is inactive")
	fs.BoolVar(&failed, "failed", failed, "wait until the unit is failed")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("systemd requires exactly one UNIT")
	}
	state, err := parseSystemdState(active, inactive, failed)
	if err != nil {
		return nil, err
	}
	cond := condition.NewSystemd(args[0])
	cond.State = state
	return cond, nil
}

func parseSystemdState(active, inactive, failed bool) (condition.SystemdState, error) {
	set := 0
	for _, value := range []bool{active, inactive, failed} {
		if value {
			set++
		}
	}
	if set > 1 {
		return "", fmt.Errorf("--active, --inactive, and --failed are mutually exclusive")
	}
	switch {
	case inactive:
		return condition.SystemdInactive, nil
	case failed:
		return condition.SystemdFailed, nil
	default:
		return condition.SystemdActive, nil
	}
}

func parseLaunchdCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("launchd", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	loaded := false
	running := false
	fs.BoolVar(&loaded, "loaded", loaded, "wait until the service is loaded")
	fs.BoolVar(&running, "running", running, "wait until the service is running")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("launchd requires exactly one LABEL")
	}
	state, err := parseLaunchdState(loaded, running)
	if err != nil {
		return nil, err
	}
	cond := condition.NewLaunchd(fs.Args()[0])
	cond.State = state
	return cond, nil
}

func parseLaunchdState(loaded, running bool) (condition.LaunchdState, error) {
	if loaded && running {
		return "", fmt.Errorf("--loaded and --running are mutually exclusive")
	}
	if loaded {
		return condition.LaunchdLoaded, nil
	}
	return condition.LaunchdRunning, nil
}

func parsePIDFileCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("pidfile", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	running := false
	stopped := false
	fs.BoolVar(&running, "running", running, "wait until the pidfile points to a running process")
	fs.BoolVar(&stopped, "stopped", stopped, "wait until the pidfile is absent or stale")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("pidfile requires exactly one PATH")
	}
	state, err := parseProcessState(running, stopped)
	if err != nil {
		return nil, err
	}
	cond := condition.NewPIDFile(fs.Args()[0])
	cond.State = state
	return cond, nil
}

func parseLockfileCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("lockfile", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	present := false
	absent := false
	olderThanText := ""
	fs.BoolVar(&present, "present", present, "wait until the lockfile exists")
	fs.BoolVar(&absent, "absent", absent, "wait until the lockfile is absent")
	fs.StringVar(&olderThanText, "older-than", olderThanText, "wait until the lockfile exists and is at least this old")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("lockfile requires exactly one PATH")
	}
	state, err := parseLockfileState(present || olderThanText != "", absent)
	if err != nil {
		return nil, err
	}
	cond := condition.NewLockfile(fs.Args()[0])
	cond.State = state
	if olderThanText != "" {
		olderThan, err := time.ParseDuration(olderThanText)
		if err != nil {
			return nil, fmt.Errorf("invalid --older-than: %w", err)
		}
		cond.OlderThan = olderThan
	}
	return cond, nil
}

func parseLockfileState(present, absent bool) (condition.LockfileState, error) {
	if present && absent {
		return "", fmt.Errorf("--present and --absent are mutually exclusive")
	}
	if present {
		return condition.LockfilePresent, nil
	}
	return condition.LockfileAbsent, nil
}

func parsePermissionCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("permission", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	modeText := ""
	uid := -1
	gid := -1
	userName := ""
	groupName := ""
	typeText := string(condition.PermissionAny)
	fs.StringVar(&modeText, "mode", modeText, "required permission mode, such as 0644")
	fs.IntVar(&uid, "uid", uid, "required numeric owner uid")
	fs.IntVar(&gid, "gid", gid, "required numeric group gid")
	fs.StringVar(&userName, "user", userName, "required owner user name")
	fs.StringVar(&groupName, "group", groupName, "required group name")
	fs.StringVar(&typeText, "type", typeText, "required path type: any, file, dir, or symlink")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("permission requires exactly one PATH")
	}
	cond := condition.NewPermission(fs.Args()[0])
	if err := applyPermissionOptions(cond, modeText, uid, gid, userName, groupName, typeText); err != nil {
		return nil, err
	}
	return cond, nil
}

func applyPermissionOptions(cond *condition.PermissionCondition, modeText string, uid, gid int, userName, groupName, typeText string) error {
	if modeText != "" {
		mode, err := strconv.ParseUint(modeText, 8, 32)
		if err != nil {
			return fmt.Errorf("invalid --mode %q: %w", modeText, err)
		}
		cond.Mode = os.FileMode(mode)
	}
	cond.Type = condition.PermissionPathType(strings.ToLower(typeText))
	if err := applyOwnerOption(cond, uid, userName); err != nil {
		return err
	}
	return applyGroupOption(cond, gid, groupName)
}

func applyOwnerOption(cond *condition.PermissionCondition, uid int, userName string) error {
	if uid >= 0 && userName != "" {
		return fmt.Errorf("--uid and --user are mutually exclusive")
	}
	if uid >= 0 {
		cond.UID = &uid
		return nil
	}
	if userName == "" {
		return nil
	}
	parsed, err := strconv.Atoi(userName)
	if err != nil {
		return fmt.Errorf("--user must be a numeric uid")
	}
	cond.UID = &parsed
	return nil
}

func applyGroupOption(cond *condition.PermissionCondition, gid int, groupName string) error {
	if gid >= 0 && groupName != "" {
		return fmt.Errorf("--gid and --group are mutually exclusive")
	}
	if gid >= 0 {
		cond.GID = &gid
		return nil
	}
	if groupName == "" {
		return nil
	}
	parsed, err := strconv.Atoi(groupName)
	if err != nil {
		return fmt.Errorf("--group must be a numeric gid")
	}
	cond.GID = &parsed
	return nil
}

func parseChecksumCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("checksum", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	algorithm := string(condition.ChecksumAuto)
	expected := ""
	fs.StringVar(&algorithm, "algorithm", algorithm, "hash algorithm: auto, sha1, sha256, or sha512")
	fs.StringVar(&expected, "equals", expected, "expected hex checksum, optionally prefixed with algorithm:")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("checksum requires exactly one PATH")
	}
	cond := condition.NewChecksum(fs.Args()[0])
	cond.Algorithm = condition.ChecksumAlgorithm(strings.ToLower(algorithm))
	cond.Expected = expected
	return cond, nil
}

func parseArchiveCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("archive", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	member := ""
	matches := ""
	format := string(condition.ArchiveAuto)
	fs.StringVar(&member, "contains", member, "required member path inside the archive")
	fs.StringVar(&matches, "matches", matches, "required archive member glob")
	fs.StringVar(&format, "format", format, "archive format: auto, tar, tgz, or zip")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("archive requires exactly one PATH")
	}
	cond := condition.NewArchive(fs.Args()[0])
	cond.Member = member
	cond.Matches = matches
	cond.Format = condition.ArchiveFormat(strings.ToLower(format))
	return cond, nil
}

func parseCosignCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("cosign", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	blob := false
	key := ""
	signature := ""
	certificate := ""
	identity := ""
	oidcIssuer := ""
	fs.BoolVar(&blob, "blob", blob, "verify an artifact blob with cosign verify-blob")
	fs.StringVar(&key, "key", key, "public key path or URI")
	fs.StringVar(&signature, "signature", signature, "signature path for --blob")
	fs.StringVar(&certificate, "certificate", certificate, "certificate path for keyless verification")
	fs.StringVar(&identity, "certificate-identity", identity, "expected certificate identity for keyless verification")
	fs.StringVar(&oidcIssuer, "certificate-oidc-issuer", oidcIssuer, "expected OIDC issuer for keyless verification")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("cosign requires exactly one TARGET")
	}
	cond := condition.NewCosign(fs.Args()[0])
	cond.Key = key
	cond.Signature = signature
	cond.Certificate = certificate
	cond.Identity = identity
	cond.OIDCIssuer = oidcIssuer
	if blob {
		cond.Mode = condition.CosignBlob
	}
	return cond, nil
}

func parseNTPCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("ntp", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	maxOffsetText := ""
	timeoutText := ""
	fs.StringVar(&maxOffsetText, "max-offset", maxOffsetText, "maximum absolute clock offset, such as 250ms")
	fs.StringVar(&timeoutText, "timeout", timeoutText, "per-attempt NTP query timeout")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("ntp requires exactly one HOST[:PORT]")
	}
	cond := condition.NewNTP(fs.Args()[0])
	if maxOffsetText != "" {
		maxOffset, err := time.ParseDuration(maxOffsetText)
		if err != nil {
			return nil, fmt.Errorf("invalid --max-offset: %w", err)
		}
		cond.MaxOffset = maxOffset
	}
	if timeoutText != "" {
		timeout, err := time.ParseDuration(timeoutText)
		if err != nil {
			return nil, fmt.Errorf("invalid --timeout: %w", err)
		}
		cond.AttemptTimeout = timeout
	}
	return cond, nil
}

func parseICMPCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("icmp", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	count := 1
	timeoutText := ""
	fs.IntVar(&count, "count", count, "number of echo requests to send")
	fs.StringVar(&timeoutText, "timeout", timeoutText, "per-attempt ping timeout")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("icmp requires exactly one HOST")
	}
	cond := condition.NewICMP(fs.Args()[0])
	cond.Count = count
	if timeoutText != "" {
		timeout, err := time.ParseDuration(timeoutText)
		if err != nil {
			return nil, fmt.Errorf("invalid --timeout: %w", err)
		}
		cond.AttemptTimeout = timeout
	}
	return cond, nil
}

func parseGRPCCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("grpc", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	service := ""
	status := string(condition.GRPCStatusServing)
	useTLS := false
	timeoutText := ""
	fs.StringVar(&service, "service", service, "gRPC health service name")
	fs.StringVar(&status, "status", status, "expected health status: SERVING, NOT_SERVING, UNKNOWN, or SERVICE_UNKNOWN")
	fs.BoolVar(&useTLS, "tls", useTLS, "use TLS for host:port addresses")
	fs.StringVar(&timeoutText, "timeout", timeoutText, "per-attempt gRPC health check timeout")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("grpc requires exactly one ADDRESS")
	}
	cond := condition.NewGRPC(fs.Args()[0])
	cond.Service = service
	cond.Status = condition.GRPCServingStatus(strings.ToUpper(status))
	cond.UseTLS = useTLS
	if timeoutText != "" {
		timeout, err := time.ParseDuration(timeoutText)
		if err != nil {
			return nil, fmt.Errorf("invalid --timeout: %w", err)
		}
		cond.AttemptTimeout = timeout
	}
	return cond, nil
}

func parseWebSocketCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("websocket", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	send := ""
	contains := ""
	matches := ""
	timeoutText := ""
	headers := []string{}
	fs.StringVar(&send, "send", send, "text message to send after connecting")
	fs.StringVar(&contains, "contains", contains, "required text message substring")
	fs.StringVar(&matches, "matches", matches, "required text message regex")
	fs.StringVar(&timeoutText, "timeout", timeoutText, "per-attempt WebSocket timeout")
	fs.StringArrayVar(&headers, "header", headers, "extra HTTP header, as Key: Value or Key=Value")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 1 {
		return nil, fmt.Errorf("websocket requires exactly one URL")
	}
	cond := condition.NewWebSocket(fs.Args()[0])
	cond.Send = send
	cond.Contains = contains
	if matches != "" {
		re, err := regexp.Compile(matches)
		if err != nil {
			return nil, fmt.Errorf("invalid --matches regex: %w", err)
		}
		cond.Matches = re
	}
	parsedHeaders, err := parseHTTPHeaders(headers)
	if err != nil {
		return nil, err
	}
	cond.Headers = parsedHeaders
	if timeoutText != "" {
		timeout, err := time.ParseDuration(timeoutText)
		if err != nil {
			return nil, fmt.Errorf("invalid --timeout: %w", err)
		}
		cond.AttemptTimeout = timeout
	}
	return cond, nil
}

func validDockerStatus(status string) bool {
	switch status {
	case "any", "created", "running", "paused", "restarting", "removing", "exited", "dead":
		return true
	default:
		return false
	}
}

func validDockerHealth(health string) bool {
	switch health {
	case "", "healthy", "unhealthy", "starting", "none":
		return true
	default:
		return false
	}
}

func parseFileCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("file", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	contains := ""
	existsFlag := false
	deletedFlag := false
	nonemptyFlag := false
	fs.StringVar(&contains, "contains", "", "required file substring")
	fs.BoolVar(&existsFlag, "exists", false, "wait until the file exists")
	fs.BoolVar(&deletedFlag, "deleted", false, "wait until the file is deleted")
	fs.BoolVar(&nonemptyFlag, "nonempty", false, "wait until the file is non-empty")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("file requires exactly one PATH")
	}
	state, err := parseFileState(existsFlag, deletedFlag, nonemptyFlag, contains)
	if err != nil {
		return nil, err
	}
	cond := condition.NewFile(args[0], state)
	cond.Contains = contains
	return cond, nil
}

func parseFileState(existsFlag, deletedFlag, nonemptyFlag bool, contains string) (condition.FileState, error) {
	set := 0
	if existsFlag {
		set++
	}
	if deletedFlag {
		set++
	}
	if nonemptyFlag {
		set++
	}
	if set > 1 {
		return "", fmt.Errorf("--exists, --deleted, and --nonempty are mutually exclusive")
	}
	if deletedFlag && contains != "" {
		return "", fmt.Errorf("--deleted cannot be combined with --contains")
	}
	state := condition.FileExists
	switch {
	case deletedFlag:
		state = condition.FileDeleted
	case nonemptyFlag:
		state = condition.FileNonEmpty
	}
	return state, nil
}

func parseLogCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("log", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	contains, matches, exclude, jsonpath := "", "", "", ""
	fromStart := false
	tail, minMatches := 0, 1
	fs.StringVar(&contains, "contains", "", "required line substring")
	fs.StringVar(&matches, "matches", "", "required line regex")
	fs.StringVar(&exclude, "exclude", "", "skip lines matching this regex before applying other matchers")
	fs.StringVar(&jsonpath, "jsonpath", "", "JSON expression evaluated on each line")
	fs.BoolVar(&fromStart, "from-start", false, "scan from beginning of file (default: skip existing content)")
	fs.IntVar(&tail, "tail", 0, "scan last N lines of existing content before tailing new lines")
	fs.IntVar(&minMatches, "min-matches", 1, "number of cumulative matching lines required")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("log requires exactly one PATH")
	}
	if err := validateLogOptions(contains, matches, jsonpath, exclude, tail, minMatches, fromStart); err != nil {
		return nil, err
	}
	return buildLogCondition(args[0], contains, matches, jsonpath, exclude, fromStart, tail, minMatches)
}

func validateLogOptions(contains, matches, jsonpath, exclude string, tail, minMatches int, fromStart bool) error {
	if contains == "" && matches == "" && jsonpath == "" {
		return fmt.Errorf("log requires at least one of --contains, --matches, or --jsonpath")
	}
	if fromStart && tail > 0 {
		return fmt.Errorf("--from-start and --tail are mutually exclusive")
	}
	if minMatches < 1 {
		return fmt.Errorf("--min-matches must be at least 1")
	}
	return nil
}

func buildLogCondition(path, contains, matches, jsonpath, exclude string, fromStart bool, tail, minMatches int) (condition.Condition, error) {
	cond := condition.NewLog(path)
	cond.Contains = contains
	cond.FromStart = fromStart
	cond.Tail = tail
	cond.MinMatches = minMatches
	if matches != "" {
		re, err := regexp.Compile(matches)
		if err != nil {
			return nil, fmt.Errorf("invalid --matches regex: %w", err)
		}
		cond.Regex = re
	}
	if exclude != "" {
		re, err := regexp.Compile(exclude)
		if err != nil {
			return nil, fmt.Errorf("invalid --exclude regex: %w", err)
		}
		cond.Exclude = re
	}
	if jsonpath != "" {
		e, err := expr.Compile(jsonpath)
		if err != nil {
			return nil, err
		}
		cond.JSONExpr = e
	}
	return cond, nil
}

func parseKubernetesCondition(segment []string) (condition.Condition, error) {
	fs := pflag.NewFlagSet("k8s", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	namespace := "default"
	conditionName := ""
	jsonpath := ""
	waitFor := ""
	selector := ""
	kubeconfig := ""
	all := false
	fs.StringVar(&namespace, "namespace", namespace, "namespace")
	fs.StringVar(&conditionName, "condition", conditionName, "condition type")
	fs.StringVar(&jsonpath, "jsonpath", jsonpath, "JSON expression")
	fs.StringVar(&waitFor, "for", waitFor, "typed wait: ready|rollout|complete")
	fs.StringVar(&selector, "selector", selector, "label selector for kind-level waits")
	fs.StringVar(&kubeconfig, "kubeconfig", kubeconfig, "kubeconfig path")
	fs.BoolVar(&all, "all", all, "require all selected resources to satisfy --for")
	if err := fs.Parse(segment[1:]); err != nil {
		return nil, err
	}
	args := fs.Args()
	if len(args) != 1 {
		return nil, fmt.Errorf("k8s requires exactly one RESOURCE (e.g. pod/myapp, deployment/api)")
	}
	if err := validateKubernetesOptions(args[0], conditionName, jsonpath, waitFor, selector, all); err != nil {
		return nil, err
	}
	jsonExpr, err := compileKubernetesJSONExpr(jsonpath)
	if err != nil {
		return nil, err
	}
	cond := condition.NewKubernetes(args[0])
	cond.Namespace = namespace
	cond.Condition = conditionName
	cond.WaitFor = waitFor
	cond.Selector = selector
	cond.All = all
	cond.JSONExpr = jsonExpr
	cond.Kubeconfig = kubeconfig
	return cond, nil
}

func validateKubernetesOptions(resource, conditionName, jsonpath, waitFor, selector string, all bool) error {
	if err := validateKubernetesMatcherOptions(conditionName, jsonpath, waitFor); err != nil {
		return err
	}
	if err := validateKubernetesSelectorOptions(resource, selector, waitFor, all); err != nil {
		return err
	}
	return validateKubernetesWaitKind(resource, selector, waitFor)
}

func validateKubernetesMatcherOptions(conditionName, jsonpath, waitFor string) error {
	if conditionName != "" && jsonpath != "" {
		return fmt.Errorf("--condition and --jsonpath are mutually exclusive")
	}
	if waitFor != "" && (conditionName != "" || jsonpath != "") {
		return fmt.Errorf("--for is mutually exclusive with --condition and --jsonpath")
	}
	if waitFor != "" && !validKubernetesWaitFor(waitFor) {
		return fmt.Errorf("invalid kubernetes --for value %q", waitFor)
	}
	return nil
}

func validateKubernetesSelectorOptions(resource, selector, waitFor string, all bool) error {
	switch {
	case selector != "" && waitFor == "":
		return fmt.Errorf("--selector requires --for")
	case all && selector == "":
		return fmt.Errorf("--all requires --selector")
	case selector != "" && strings.Contains(resource, "/"):
		return fmt.Errorf("--selector requires a resource kind without /name syntax")
	default:
		return validateKubernetesSelector(selector)
	}
}

func validateKubernetesSelector(selector string) error {
	if selector == "" {
		return nil
	}
	if _, err := labels.Parse(selector); err != nil {
		return fmt.Errorf("invalid kubernetes selector: %w", err)
	}
	return nil
}

func validateKubernetesWaitKind(resource, selector, waitFor string) error {
	if waitFor == "" {
		return nil
	}
	kind := kubernetesResourceKind(resource, selector)
	if kubernetesWaitSupportsKind(waitFor, kind) {
		return nil
	}
	return fmt.Errorf("--for %s is not supported for kubernetes resource kind %q", waitFor, kind)
}

func kubernetesResourceKind(resource, selector string) string {
	if selector != "" {
		return strings.ToLower(resource)
	}
	kind, _, _ := strings.Cut(resource, "/")
	return strings.ToLower(kind)
}

func kubernetesWaitSupportsKind(waitFor, kind string) bool {
	switch waitFor {
	case "ready":
		return kubernetesKindIs(kind, "pod")
	case "complete":
		return kubernetesKindIs(kind, "job")
	case "rollout":
		return kubernetesKindIs(kind, "deployment") || kubernetesKindIs(kind, "statefulset") || kubernetesKindIs(kind, "daemonset")
	default:
		return false
	}
}

func kubernetesKindIs(kind, canonical string) bool {
	return normalizeKubernetesKind(kind) == canonical
}

func normalizeKubernetesKind(kind string) string {
	switch strings.ToLower(kind) {
	case "pod", "pods", "po":
		return "pod"
	case "deployment", "deployments", "deploy":
		return "deployment"
	case "statefulset", "statefulsets", "sts":
		return "statefulset"
	case "daemonset", "daemonsets", "ds":
		return "daemonset"
	case "job", "jobs":
		return "job"
	default:
		return strings.ToLower(kind)
	}
}

func compileKubernetesJSONExpr(jsonpath string) (*expr.Expression, error) {
	if jsonpath != "" {
		jsonExpr, err := expr.Compile(jsonpath)
		if err != nil {
			return nil, err
		}
		return jsonExpr, nil
	}
	return nil, nil
}

func validKubernetesWaitFor(waitFor string) bool {
	switch waitFor {
	case "ready", "rollout", "complete":
		return true
	default:
		return false
	}
}

func validateEnvVars(env []string) error {
	for _, e := range env {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("--env must use KEY=VALUE")
		}
	}
	return nil
}

func validateExecOptions(env []string, exitCode int, maxOutputBytes int64) error {
	if err := validateEnvVars(env); err != nil {
		return err
	}
	if exitCode < 0 {
		return fmt.Errorf("--exit-code cannot be negative")
	}
	if maxOutputBytes <= 0 {
		return fmt.Errorf("--max-output-bytes must be positive")
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
	maxOutputBytes := condition.DefaultMaxOutputBytes
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
		return nil, fmt.Errorf("exec flags must precede --")
	}
	if err := validateExecOptions(env, exitCode, maxOutputBytes); err != nil {
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

func isSeparatorBefore(args []string, i int, current []string) bool {
	if args[i] != "--" || i+1 >= len(args) || !isConditionStart(args, i+1) {
		return false
	}
	if isValueForPreviousFlag(args, i) {
		return false
	}
	return !isExecCommandSeparator(current)
}

func isValueForPreviousFlag(args []string, i int) bool {
	if i == 0 {
		return false
	}
	prev := args[i-1]
	if strings.Contains(prev, "=") {
		return false
	}
	return conditionValueFlags[prev]
}

var conditionValueFlags = map[string]bool{
	"--method":            true,
	"--status":            true,
	"--header":            true,
	"--body":              true,
	"--body-file":         true,
	"--body-contains":     true,
	"--body-matches":      true,
	"--jsonpath":          true,
	"--type":              true,
	"--resolver":          true,
	"--contains":          true,
	"--matches":           true,
	"--exclude":           true,
	"--tail":              true,
	"--min-matches":       true,
	"--equals":            true,
	"--min-count":         true,
	"--max-count":         true,
	"--absent-mode":       true,
	"--server":            true,
	"--rcode":             true,
	"--transport":         true,
	"--udp-size":          true,
	"--servername":        true,
	"--valid-for":         true,
	"--ca-file":           true,
	"--banner-contains":   true,
	"--user":              true,
	"--password":          true,
	"--host-key-sha256":   true,
	"--metadata":          true,
	"--range":             true,
	"--endpoint-url":      true,
	"--region":            true,
	"--access-key-id":     true,
	"--secret-access-key": true,
	"--session-token":     true,
	"--health":            true,
	"--pid":               true,
	"--namespace":         true,
	"--condition":         true,
	"--for":               true,
	"--selector":          true,
	"--kubeconfig":        true,
	"--exit-code":         true,
	"--output-contains":   true,
	"--cwd":               true,
	"--env":               true,
	"--max-output-bytes":  true,
	"--name":              true,
}

func isExecCommandSeparator(current []string) bool {
	if len(current) == 0 || current[0] != "exec" {
		return false
	}
	return indexOf(current[1:], "--") < 0
}

func splitConditionSegments(args []string) ([][]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("missing condition")
	}
	if args[0] == "--" {
		return nil, fmt.Errorf("empty condition before --")
	}
	if args[len(args)-1] == "--" && !isValueForPreviousFlag(args, len(args)-1) {
		return nil, fmt.Errorf("empty trailing condition")
	}
	var segments [][]string
	var current []string
	for i := 0; i < len(args); i++ {
		if isSeparatorBefore(args, i, current) {
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

func isConditionStart(args []string, i int) bool {
	if isBackend(args[i]) {
		return true
	}
	return args[i] == "guard" && i+1 < len(args) && isBackend(args[i+1])
}

func isBackend(arg string) bool {
	_, ok := backendParsers[arg]
	return ok
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
	if out.MaxInterval != out.Interval {
		report.MaxIntervalSeconds = output.Seconds(out.MaxInterval)
	}
	if out.Backoff != "" && out.Backoff != runner.BackoffConstant {
		report.Backoff = string(out.Backoff)
	}
	if out.Jitter > 0 {
		report.Jitter = out.Jitter
	}
	if out.PerAttemptTimeout > 0 {
		report.PerAttemptTimeoutSeconds = output.Seconds(out.PerAttemptTimeout)
	}
	if out.RequiredSuccesses > 1 {
		report.RequiredSuccesses = out.RequiredSuccesses
	}
	if out.StableFor > 0 {
		report.StableForSeconds = output.Seconds(out.StableFor)
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
			Guard:          rec.Guard,
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

func readFileLimit(path string, limit int64) ([]byte, error) {
	file, info, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func helpText() string {
	return `waitfor - semantic condition poller

Usage:
  waitfor [flags] <backend> <target> [backend-flags]
  waitfor [flags] <backend> ... -- <backend> ...

Global flags:
  --timeout duration       Global deadline (default: 5m)
  --interval duration      Poll interval (default: 2s)
  --backoff constant|exponential
                           Poll backoff strategy (default: constant)
  --max-interval duration  Maximum interval for exponential backoff (default: --interval)
  --jitter percent         Poll jitter, for example 20% or 0.2 (default: 0%)
  --attempt-timeout duration
                           Per-attempt deadline; 0 disables (default: 0)
  --successes N            Consecutive successful checks required (default: 1)
  --stable-for duration    Required continuous success duration (default: 0)
  --output text|json       Output format (default: text); JSON goes to stdout
  --mode all|any           Condition mode (default: all)
  --verbose                Show each poll attempt

Condition flag:
  --name label             Human-readable condition label for text and JSON output
                           For process, --name selects the executable instead.

Doctor:
  waitfor doctor [--output text|json] [--require check]
  --require check          Require temp|shell|docker|k8s|dns-wire (repeatable or comma-separated)

HTTP:
  waitfor http [flags] URL
  --status 200|2xx         Expected status code or class (default: 200)
  --method GET             HTTP method (default: GET)
  --body text              Request body string
  --body-file path         Request body from file, capped at 10 MiB (mutually exclusive with --body)
  --body-contains text     Required response body substring
  --body-matches regex     Required response body regex
  --jsonpath expr          Required JSON expression on response body
  --header Key=Value       Request header (repeatable; Key: Value also accepted)
  --insecure               Skip TLS certificate verification
  --no-follow-redirects    Do not follow HTTP redirects

TCP:
  waitfor tcp HOST:PORT

Unix Socket:
  waitfor unix PATH

Ports:
  waitfor ports [flags] HOST
  --range START-END        Port range to check
  --any                    Succeed when any port in the range is open
  --all                    Succeed when every port in the range is open (default)

TLS:
  waitfor tls [flags] HOST:PORT
  --servername name        TLS server name for SNI and SAN verification (default: HOST)
  --valid-for duration     Minimum remaining certificate validity, such as 720h or 30d
  --ca-file path           PEM CA bundle to trust in addition to system roots

SSH:
  waitfor ssh [flags] HOST:PORT
  --banner-contains text   Required SSH banner substring
  --user name              SSH username for password auth handshake
  --password value         SSH password for auth handshake
  --host-key-sha256 fp     Required SSH host key SHA256 fingerprint, such as SHA256:...

S3:
  waitfor s3 [flags] s3://bucket[/key]
  --exists                 Wait until the bucket or object exists (default)
  --metadata Key=Value     Required object metadata from x-amz-meta-Key (repeatable)
  --contains text          Required object content substring, capped at 10 MiB
  --endpoint-url URL       S3-compatible endpoint URL, including MinIO, R2, and Ceph RGW;
                           uses path-style requests by default
  --virtual-hosted-style   Use bucket.endpoint host style with --endpoint-url
  --region name            AWS region or S3-compatible signing region
                           (default: AWS_REGION, AWS_DEFAULT_REGION, or us-east-1)
  --access-key-id value    AWS access key id (default: AWS_ACCESS_KEY_ID)
  --secret-access-key value
                           AWS secret access key (default: AWS_SECRET_ACCESS_KEY)
  --session-token value    AWS session token (default: AWS_SESSION_TOKEN)

DNS:
  waitfor dns [flags] HOST
  --type A|AAAA|CNAME|TXT|ANY|MX|SRV|NS|CAA|HTTPS|SVCB
                           DNS record type (default: A; MX/SRV/NS/CAA/HTTPS/SVCB require --resolver wire)
  --resolver system|wire   Resolver mode (default: system)
  --contains text          Required record substring
  --equals value           Required exact record value (repeatable)
  --min-count N            Minimum answer count
  --absent                 Wait until the record is absent
  --absent-mode any|nxdomain|nodata
                           Wire absence mode (default: any)
  --server address         DNS server address; port defaults to 53
  --rcode code             Wire response code, such as NOERROR or NXDOMAIN
  --transport udp|tcp      Wire transport (default: udp; truncated UDP retries over TCP)
  --edns0                  Enable EDNS0 for wire resolver
  --udp-size N             Wire EDNS0 UDP payload size

Docker:
  waitfor docker [flags] CONTAINER
  --status running         Container status: any|created|running|paused|restarting|removing|exited|dead
  --health healthy         Container health: healthy|unhealthy|starting|none

Process:
  waitfor process (--pid PID | --name NAME) [--running|--stopped]
  --pid PID                Process ID to check
  --name name              Process executable name to check
  --running                Wait until the process is running (default)
  --stopped                Wait until the process is stopped

Systemd:
  waitfor systemd UNIT [--active|--inactive|--failed]
  --active                 Wait until the unit is active (default)
  --inactive               Wait until the unit is inactive
  --failed                 Wait until the unit is failed

Launchd:
  waitfor launchd LABEL [--running|--loaded]
  --running                Wait until launchctl reports a non-zero pid (default)
  --loaded                 Wait until launchctl can print the service

PID file:
  waitfor pidfile PATH [--running|--stopped]
  --running                Wait until the pidfile points to a live process (default)
  --stopped                Wait until the pidfile is absent or stale

Lockfile:
  waitfor lockfile PATH [--absent|--present] [--older-than DURATION]
  --absent                 Wait until the lockfile is absent (default)
  --present                Wait until the lockfile exists
  --older-than duration    Wait until the lockfile exists and is at least this old

Permission:
  waitfor permission PATH [--mode 0644] [--uid UID|--user UID] [--gid GID|--group GID] [--type any|file|dir|symlink]

Checksum:
  waitfor checksum PATH --equals [ALGORITHM:]HEX [--algorithm auto|sha256|sha512|sha1]

Archive:
  waitfor archive PATH (--contains MEMBER | --matches GLOB) [--format auto|tar|tgz|zip]

Cosign:
  waitfor cosign IMAGE [--key KEY] [--certificate CERT] [--certificate-identity ID] [--certificate-oidc-issuer URL]
  waitfor cosign --blob FILE --signature SIG [--key KEY] [--certificate CERT]

NTP:
  waitfor ntp HOST[:PORT] [--max-offset DURATION] [--timeout DURATION]

ICMP:
  waitfor icmp HOST [--count N] [--timeout DURATION]

gRPC:
  waitfor grpc ADDRESS [--service NAME] [--status SERVING|NOT_SERVING|UNKNOWN|SERVICE_UNKNOWN] [--tls] [--timeout DURATION]

WebSocket:
  waitfor websocket ws://HOST/PATH [--send TEXT] [--contains TEXT|--matches REGEX] [--header Key=Value] [--timeout DURATION]

Exec:
  waitfor exec [flags] -- COMMAND [ARGS...]
  --exit-code 0            Expected exit code (default: 0)
  --output-contains text   Required stdout/stderr substring
  --jsonpath expr          Required JSON expression on stdout
  --cwd path               Working directory for the command
  --env KEY=VALUE          Extra environment variable (repeatable)
  --max-output-bytes N     Capture at most N bytes of output (default: 1048576)

File:
  waitfor file [flags] PATH
  --exists                 Wait until the file exists (default when no state flag given)
  --deleted                Wait until the file is deleted
  --nonempty               Wait until the file is non-empty
  --contains text          Required file content substring in first 10 MiB (only with --exists/--nonempty)

Glob:
  waitfor glob [flags] PATTERN
  --min-count N            Minimum number of matching files (default: 1)
  --max-count N            Maximum number of matching files (-1 disables)
  --absent                 Wait until no files match

Log:
  waitfor log [flags] PATH
  --contains text          Required line substring
  --matches regex          Required line regex
  --exclude regex          Skip lines matching this regex before applying other matchers
  --jsonpath expr          JSON expression evaluated on each line
  --from-start             Scan from beginning of file (default: skip existing content, tail new lines)
  --tail N                 Scan last N lines of existing content before tailing (mutually exclusive with --from-start)
  --min-matches N          Number of cumulative matching lines required (default: 1)

Kubernetes:
  waitfor k8s [flags] RESOURCE
  RESOURCE format: kind/name, or kind with --selector
  --condition type         Condition type to check (default: Ready)
  --jsonpath expr          JSON expression on the resource (mutually exclusive with --condition)
  --for ready|rollout|complete
                           Typed wait for pods, workloads, or jobs
  --selector labels        Label selector for kind-level waits
  --all                    Require every selected resource to satisfy --for
  --namespace ns           Namespace (default: default)
  --kubeconfig path        Path to kubeconfig file

Examples:
  waitfor http https://api.example.com/health --status 200
  waitfor tcp localhost:5432
  waitfor unix /var/run/docker.sock
  waitfor ports localhost --range 8000-8010 --any
  waitfor tls api.example.com:443 --valid-for 30d
  waitfor ssh host.example.com:22
  waitfor ssh host.example.com:22 --user deploy --password "$SSH_PASSWORD" --host-key-sha256 SHA256:...
  waitfor s3 s3://bucket/path/ready.json --exists
  waitfor s3 s3://bucket/path/ready.json --contains '"ready":true' --endpoint-url http://localhost:9000
  waitfor s3 s3://bucket/path/ready.json --endpoint-url https://ceph-rgw.example.com --region default
  waitfor dns api.internal --type A
  waitfor docker postgres --health healthy
  waitfor process --name postgres --running
  waitfor systemd nginx.service --active
  waitfor launchd system/com.example.agent --running
  waitfor pidfile /var/run/app.pid --running
  waitfor lockfile /var/run/app.lock --older-than 5m
  waitfor permission /var/run/app.conf --mode 0640 --gid 1000 --type file
  waitfor checksum dist/app.tar.gz --equals "sha256:$SHA256"
  waitfor archive dist/app.tar.gz --matches 'bin/*'
  waitfor cosign ghcr.io/org/app:v1.2.3 --certificate-identity "$IDENTITY"
  waitfor ntp time.cloudflare.com --max-offset 250ms --timeout 1s
  waitfor icmp 192.0.2.10 --count 3 --timeout 1s
  waitfor grpc localhost:50051 --service grpc.health.v1.Health --tls --timeout 2s
  waitfor websocket ws://localhost:8080/events --matches 'ready|ok' --header 'Authorization=Bearer ...'
  waitfor file /tmp/ready.flag --exists
  waitfor glob '/tmp/jobs/*.done' --min-count 5
  waitfor log /var/log/app.log --contains "server ready"
  waitfor log /var/log/app.log --matches "ERROR:.*timeout" --from-start
  waitfor exec --output-contains Running -- kubectl get pod myapp
  waitfor k8s deployment/api --condition Available
  waitfor k8s deployment/api --for rollout
  waitfor k8s pod --selector app=api --for ready --all
  waitfor http https://api.example.com/health -- guard log /var/log/app.log --matches "FATAL|panic"
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
