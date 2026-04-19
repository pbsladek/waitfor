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
	method := c.Method
	if method == "" {
		method = http.MethodGet
	}
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

	// Always read the full body (capped at 10 MB) so the connection is
	// returned to the keep-alive pool. Closing without draining discards
	// the connection on every poll cycle.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return Unsatisfied("", err)
	}

	if !statusMatcher.Match(resp.StatusCode) {
		detail := fmt.Sprintf("status %d, expected %s", resp.StatusCode, statusMatcher.String())
		return Unsatisfied(detail, errors.New(detail))
	}

	return c.checkResponseBody(body, resp.StatusCode)
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
			return Fatal(err)
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
	if !c.Insecure && !c.NoRedirects {
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
		if c.NoRedirects {
			c.clientCache.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			}
		}
	})
	return c.clientCache
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
