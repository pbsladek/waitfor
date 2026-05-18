package condition

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pbsladek/wait-for/internal/expr"
)

type HTTPCondition struct {
	URL            string
	Method         string
	ExpectedStatus int
	StatusMatcher  HTTPStatusMatcher
	RequestBody    []byte
	BodyContains   string
	BodyRegex      *regexp.Regexp   // pre-compiled; use BodyRegex.String() for display
	BodyJSONExpr   *expr.Expression // pre-compiled; use BodyJSONExpr.String() for display
	Insecure       bool
	NoRedirects    bool
	Headers        map[string]string
	Client         *http.Client
	clientOnce     sync.Once
	clientCache    *http.Client
}

type HTTPStatusMatcher struct {
	raw   string
	exact int
	class int
}

var userinfoInURLPattern = regexp.MustCompile(`://[^/\s"@]+@`)

const (
	maxHTTPBodyBytes        int64 = 10 * 1024 * 1024
	maxHTTPDrainBytes       int64 = 512 * 1024
	maxHTTPRequestBodyBytes       = 10 * 1024 * 1024
	maxHTTPMatcherBytes           = 1 * 1024 * 1024
	maxHTTPHeaderCount            = 64
	maxHTTPHeaderBytes            = 32 * 1024
)

func ParseHTTPStatusMatcher(raw string) (HTTPStatusMatcher, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "200"
	}
	if len(raw) == 3 && raw[1:] == "xx" && raw[0] >= '1' && raw[0] <= '5' {
		return HTTPStatusMatcher{raw: raw, class: int(raw[0] - '0')}, nil
	}
	code, err := strconv.Atoi(raw)
	if err != nil || code < 100 || code > 599 {
		return HTTPStatusMatcher{}, fmt.Errorf("invalid HTTP status")
	}
	return HTTPStatusMatcher{raw: raw, exact: code}, nil
}

func (m HTTPStatusMatcher) Match(code int) bool {
	if m.class != 0 {
		return code/100 == m.class
	}
	expected := m.exact
	if expected == 0 {
		expected = http.StatusOK
	}
	return code == expected
}

func (m HTTPStatusMatcher) String() string {
	if m.raw != "" {
		return m.raw
	}
	if m.exact != 0 {
		return strconv.Itoa(m.exact)
	}
	return "200"
}

func NewHTTP(url string) *HTTPCondition {
	return &HTTPCondition{
		URL:     url,
		Method:  http.MethodGet,
		Headers: map[string]string{},
	}
}

func (c *HTTPCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "http", Target: redactHTTPURL(c.URL)}
}

func (c *HTTPCondition) Check(ctx context.Context) Result {
	if err := validateHTTPConfig(c); err != nil {
		return Fatal(err)
	}
	method := c.method()
	statusMatcher := c.statusMatcher()

	var reqBody io.Reader
	if len(c.RequestBody) > 0 {
		reqBody = bytes.NewReader(c.RequestBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL, reqBody)
	if err != nil {
		return Fatal(redactHTTPError(err))
	}
	for key, value := range c.Headers {
		req.Header.Set(key, value)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return Unsatisfied("", redactHTTPError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := c.responseBodyForCheck(resp.Body)
	if err != nil {
		return Unsatisfied("", err)
	}

	if !statusMatcher.Match(resp.StatusCode) {
		detail := fmt.Sprintf("status %d, expected %s", resp.StatusCode, statusMatcher.String())
		return Unsatisfied(detail, errors.New(detail))
	}

	return c.checkResponseBody(body, resp.StatusCode)
}

func (c *HTTPCondition) responseBodyForCheck(body io.Reader) ([]byte, error) {
	if c.needsResponseBody() {
		return readHTTPBody(body)
	}
	return nil, drainHTTPBody(body)
}

func (c *HTTPCondition) needsResponseBody() bool {
	return c.BodyContains != "" || c.BodyRegex != nil || c.BodyJSONExpr != nil
}

func readHTTPBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxHTTPBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxHTTPBodyBytes {
		return nil, fmt.Errorf("HTTP response body too large")
	}
	return data, nil
}

func drainHTTPBody(body io.Reader) error {
	_, err := io.Copy(io.Discard, io.LimitReader(body, maxHTTPDrainBytes))
	return err
}

func validateHTTPConfig(c *HTTPCondition) error {
	if err := validateHTTPURL(c.URL); err != nil {
		return err
	}
	if !validHTTPToken(c.method()) {
		return fmt.Errorf("invalid HTTP method")
	}
	if err := validateHTTPStatusConfig(c); err != nil {
		return err
	}
	if len(c.RequestBody) > maxHTTPRequestBodyBytes {
		return fmt.Errorf("HTTP request body is too large")
	}
	if len(c.BodyContains) > maxHTTPMatcherBytes {
		return fmt.Errorf("HTTP body substring matcher is too large")
	}
	return validateHTTPHeaders(c.Headers)
}

func validateHTTPURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid HTTP URL")
	}
	if err := validateHTTPScheme(parsed.Scheme); err != nil {
		return err
	}
	if parsed.User != nil {
		return fmt.Errorf("HTTP URL userinfo is not allowed")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("HTTP URL fragment is not allowed")
	}
	return validateHTTPAuthority(parsed)
}

func validateHTTPScheme(scheme string) error {
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("HTTP URL must use http or https")
	}
	return nil
}

func validateHTTPAuthority(parsed *url.URL) error {
	if parsed.Hostname() == "" || containsSpaceOrControl(parsed.Hostname()) {
		return fmt.Errorf("invalid HTTP URL host")
	}
	if parsed.Port() == "" && strings.Contains(parsed.Host, ":") &&
		(!strings.HasPrefix(parsed.Host, "[") || !strings.HasSuffix(parsed.Host, "]")) {
		return fmt.Errorf("invalid HTTP URL port")
	}
	if port := parsed.Port(); port != "" && !validPortNumber(port) {
		return fmt.Errorf("invalid HTTP URL port")
	}
	return nil
}

func (c *HTTPCondition) method() string {
	if c.Method == "" {
		return http.MethodGet
	}
	return c.Method
}

func validateHTTPStatusConfig(c *HTTPCondition) error {
	hasMatcher := c.StatusMatcher.raw != "" || c.StatusMatcher.exact != 0 || c.StatusMatcher.class != 0
	if c.ExpectedStatus != 0 && hasMatcher {
		return fmt.Errorf("HTTP expected status and status matcher are mutually exclusive")
	}
	if c.ExpectedStatus != 0 && !validHTTPStatusCode(c.ExpectedStatus) {
		return fmt.Errorf("invalid HTTP status")
	}
	if !hasMatcher {
		return nil
	}
	return validateHTTPStatusMatcher(c.StatusMatcher)
}

func validateHTTPStatusMatcher(m HTTPStatusMatcher) error {
	if m.raw != "" {
		_, err := ParseHTTPStatusMatcher(m.raw)
		return err
	}
	if m.exact != 0 && m.class != 0 {
		return fmt.Errorf("HTTP status matcher cannot set exact and class")
	}
	if m.exact != 0 && !validHTTPStatusCode(m.exact) {
		return fmt.Errorf("invalid HTTP status")
	}
	if m.class != 0 && (m.class < 1 || m.class > 5) {
		return fmt.Errorf("invalid HTTP status")
	}
	return nil
}

func validHTTPStatusCode(code int) bool {
	return code >= 100 && code <= 599
}

func validateHTTPHeaders(headers map[string]string) error {
	if len(headers) > maxHTTPHeaderCount {
		return fmt.Errorf("too many HTTP headers")
	}
	total := 0
	for key, value := range headers {
		if !validHTTPToken(key) {
			return fmt.Errorf("invalid HTTP header name %q", key)
		}
		if !validHTTPHeaderValue(value) {
			return fmt.Errorf("invalid HTTP header value for %q", key)
		}
		total += len(key) + len(value)
		if total > maxHTTPHeaderBytes {
			return fmt.Errorf("HTTP headers are too large")
		}
	}
	return nil
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !isHTTPTokenChar(value[i]) {
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

func (c *HTTPCondition) checkResponseBody(body []byte, statusCode int) Result {
	details := []string{fmt.Sprintf("status %d", statusCode)}
	if c.BodyContains != "" {
		if !bytes.Contains(body, []byte(c.BodyContains)) {
			return Unsatisfied("body substring not found", fmt.Errorf("body does not contain required substring"))
		}
		details = append(details, "body contains required substring")
	}
	if c.BodyRegex != nil {
		if !c.BodyRegex.Match(body) {
			return Unsatisfied("body regex not matched", fmt.Errorf("body does not match required regex"))
		}
		details = append(details, "body matches required regex")
	}
	if c.BodyJSONExpr != nil {
		ok, detail, err := c.BodyJSONExpr.EvaluateJSON(body)
		if err != nil {
			return Unsatisfied("jsonpath evaluation failed", err)
		}
		if !ok {
			return Unsatisfied(detail, fmt.Errorf("jsonpath condition not satisfied"))
		}
		details = append(details, detail)
	}
	return Satisfied(strings.Join(details, ", "))
}

func (c *HTTPCondition) statusMatcher() HTTPStatusMatcher {
	if c.StatusMatcher.raw != "" || c.StatusMatcher.exact != 0 || c.StatusMatcher.class != 0 {
		return c.StatusMatcher
	}
	if c.ExpectedStatus != 0 {
		return HTTPStatusMatcher{raw: strconv.Itoa(c.ExpectedStatus), exact: c.ExpectedStatus}
	}
	status, _ := ParseHTTPStatusMatcher("200")
	return status
}

func buildInsecureTransport() http.RoundTripper {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := base.Clone()
		cloned.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		return cloned
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
}

func (c *HTTPCondition) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	needsRedirectPolicy := len(c.Headers) > 0
	if !c.Insecure && !c.NoRedirects && !needsRedirectPolicy {
		return http.DefaultClient
	}
	c.clientOnce.Do(func() {
		transport := http.DefaultTransport
		if c.Insecure {
			transport = buildInsecureTransport()
		}
		c.clientCache = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
		configureHTTPRedirectPolicy(c.clientCache, c, needsRedirectPolicy)
	})
	return c.clientCache
}

func configureHTTPRedirectPolicy(client *http.Client, c *HTTPCondition, needsRedirectPolicy bool) {
	if c.NoRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		return
	}
	if needsRedirectPolicy {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return stripHeadersOnCrossOriginRedirect(req, via, c.Headers)
		}
	}
}

func stripHeadersOnCrossOriginRedirect(req *http.Request, via []*http.Request, headers map[string]string) error {
	if len(via) == 0 || sameHTTPOrigin(req.URL, via[0].URL) {
		return nil
	}
	for key := range headers {
		req.Header.Del(key)
	}
	return nil
}

func sameHTTPOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func redactHTTPURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.User != nil {
		parsed.User = url.User("REDACTED")
	}
	query := parsed.Query()
	changed := false
	for key := range query {
		query.Set(key, "REDACTED")
		changed = true
	}
	if changed {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

func redactHTTPError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		msg := redactHTTPErrorText(urlErr.Err.Error(), urlErr.URL)
		return fmt.Errorf("%s %q: %s", urlErr.Op, redactHTTPURL(urlErr.URL), msg)
	}
	return err
}

func redactHTTPErrorText(text, rawURL string) string {
	redactedURL := redactHTTPURL(rawURL)
	text = strings.ReplaceAll(text, rawURL, redactedURL)
	text = userinfoInURLPattern.ReplaceAllString(text, "://REDACTED@")
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return text
	}
	text = redactParsedURLUserinfo(text, parsed)
	return redactQueryValues(text, parsed)
}

func redactParsedURLUserinfo(text string, parsed *url.URL) string {
	if parsed.User != nil {
		if username := parsed.User.Username(); username != "" {
			text = strings.ReplaceAll(text, username, "REDACTED")
		}
		if password, ok := parsed.User.Password(); ok && password != "" {
			text = strings.ReplaceAll(text, password, "REDACTED")
		}
	}
	return text
}

func redactQueryValues(text string, parsed *url.URL) string {
	for _, values := range parsed.Query() {
		for _, value := range values {
			if value != "" {
				text = strings.ReplaceAll(text, value, "REDACTED")
			}
		}
	}
	return text
}
