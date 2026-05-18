package condition

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxS3ContentBytes int64 = 10 * 1024 * 1024

type S3Condition struct {
	URL                string
	EndpointURL        string
	Region             string
	VirtualHostedStyle bool
	Credentials        S3Credentials
	Metadata           map[string]string
	Contains           string
	Client             *http.Client
	Now                func() time.Time
}

type S3Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

type S3Target struct {
	Bucket string
	Key    string
}

func NewS3(rawURL string) *S3Condition {
	return &S3Condition{
		URL:      rawURL,
		Region:   "us-east-1",
		Metadata: map[string]string{},
	}
}

func (c *S3Condition) Descriptor() Descriptor {
	return Descriptor{Backend: "s3", Target: c.URL}
}

func (c *S3Condition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	target, err := validateS3Config(c)
	if err != nil {
		return Fatal(err)
	}
	req, err := c.newS3Request(ctx, target)
	if err != nil {
		return Fatal(err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Unsatisfied("", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return unsatisfiedS3Status(resp.StatusCode, target)
	}
	if result := c.checkS3Metadata(resp.Header); result != nil {
		return *result
	}
	if c.Contains != "" {
		return checkS3Content(ctx, resp.Body, c.Contains)
	}
	return Satisfied(s3SatisfiedDetail(target, c))
}

func validateS3Config(c *S3Condition) (S3Target, error) {
	target, err := ParseS3URL(c.URL)
	if err != nil {
		return S3Target{}, err
	}
	if err := validateS3ObjectChecks(c, target); err != nil {
		return S3Target{}, err
	}
	if err := validateS3Endpoint(c.EndpointURL); err != nil {
		return S3Target{}, err
	}
	if err := validateS3Credentials(c.Credentials); err != nil {
		return S3Target{}, err
	}
	if err := validateS3CredentialTransport(c.EndpointURL, c.Credentials); err != nil {
		return S3Target{}, err
	}
	return target, nil
}

func validateS3ObjectChecks(c *S3Condition, target S3Target) error {
	if c.Contains != "" && target.Key == "" {
		return fmt.Errorf("s3 content checks require an object key")
	}
	if len(c.Metadata) > 0 && target.Key == "" {
		return fmt.Errorf("s3 metadata checks require an object key")
	}
	if strings.TrimSpace(c.region()) == "" {
		return fmt.Errorf("s3 region is required")
	}
	return nil
}

func ParseS3URL(raw string) (S3Target, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "s3" || parsed.Host == "" {
		return S3Target{}, fmt.Errorf("invalid s3 URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return S3Target{}, fmt.Errorf("invalid s3 URL")
	}
	key := strings.TrimPrefix(parsed.EscapedPath(), "/")
	if key != "" {
		var err error
		key, err = url.PathUnescape(key)
		if err != nil {
			return S3Target{}, fmt.Errorf("invalid s3 object key")
		}
	}
	return S3Target{Bucket: parsed.Host, Key: key}, nil
}

func validateS3Endpoint(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid s3 endpoint URL")
	}
	return validateParsedS3Endpoint(parsed)
}

func validateParsedS3Endpoint(parsed *url.URL) error {
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

func validateS3CredentialTransport(endpoint string, creds S3Credentials) error {
	if endpoint == "" || creds.AccessKeyID == "" {
		return nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if parsed.Scheme == "http" {
		return fmt.Errorf("s3 credentials require an https endpoint")
	}
	return nil
}

func validateS3Credentials(creds S3Credentials) error {
	hasKey := creds.AccessKeyID != ""
	hasSecret := creds.SecretAccessKey != ""
	if hasKey != hasSecret {
		return fmt.Errorf("s3 access key id and secret access key must be provided together")
	}
	return nil
}

func (c *S3Condition) newS3Request(ctx context.Context, target S3Target) (*http.Request, error) {
	method := http.MethodHead
	if c.Contains != "" {
		method = http.MethodGet
	}
	endpoint, err := c.s3RequestURL(target)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "waitfor")
	if c.Credentials.AccessKeyID != "" {
		signS3Request(req, c.Credentials, c.region(), c.now())
	}
	return req, nil
}

func (c *S3Condition) s3RequestURL(target S3Target) (string, error) {
	if c.EndpointURL == "" {
		return c.awsS3URL(target), nil
	}
	parsed, err := url.Parse(c.EndpointURL)
	if err != nil {
		return "", err
	}
	if c.VirtualHostedStyle {
		parsed.Host = target.Bucket + "." + parsed.Host
		parsed.Path = joinS3Path(parsed.Path, target.Key)
	} else {
		parsed.Path = joinS3Path(parsed.Path, target.Bucket, target.Key)
	}
	return parsed.String(), nil
}

func (c *S3Condition) awsS3URL(target S3Target) string {
	u := url.URL{
		Scheme: "https",
		Host:   target.Bucket + ".s3." + c.region() + ".amazonaws.com",
		Path:   joinS3Path("", target.Key),
	}
	return u.String()
}

func joinS3Path(base string, parts ...string) string {
	path := "/" + strings.Trim(base, "/")
	if path == "/" {
		path = ""
	}
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == len(parts)-1 {
			path = appendS3ObjectKey(path, part)
			continue
		}
		path = appendS3PathSegment(path, part)
	}
	if path == "" {
		return "/"
	}
	return path
}

func appendS3PathSegment(path, segment string) string {
	segment = strings.Trim(segment, "/")
	if segment == "" {
		return path
	}
	return strings.TrimRight(path, "/") + "/" + segment
}

func appendS3ObjectKey(path, key string) string {
	return strings.TrimRight(path, "/") + "/" + key
}

func (c *S3Condition) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (c *S3Condition) region() string {
	if c.Region != "" {
		return c.Region
	}
	return "us-east-1"
}

func (c *S3Condition) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func unsatisfiedS3Status(statusCode int, target S3Target) Result {
	detail := fmt.Sprintf("s3 status %d", statusCode)
	if statusCode == http.StatusNotFound {
		if target.Key == "" {
			detail = "bucket does not exist"
		} else {
			detail = "object does not exist"
		}
	}
	return Unsatisfied(detail, errors.New(detail))
}

func (c *S3Condition) checkS3Metadata(headers http.Header) *Result {
	for key, want := range c.Metadata {
		header := s3MetadataHeader(key)
		if got := headers.Get(header); got != want {
			detail := fmt.Sprintf("metadata %s mismatch", key)
			r := Unsatisfied(detail, errors.New(detail))
			return &r
		}
	}
	return nil
}

func s3MetadataHeader(key string) string {
	key = strings.TrimSpace(key)
	key = strings.TrimPrefix(strings.ToLower(key), "x-amz-meta-")
	return "x-amz-meta-" + key
}

func checkS3Content(ctx context.Context, body io.Reader, contains string) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	data, err := io.ReadAll(io.LimitReader(body, maxS3ContentBytes+1))
	if err != nil {
		return Unsatisfied("", err)
	}
	if int64(len(data)) > maxS3ContentBytes {
		return Unsatisfied("object content too large", fmt.Errorf("object content exceeds %d bytes", maxS3ContentBytes))
	}
	if !bytes.Contains(data, []byte(contains)) {
		return Unsatisfied("object content marker not found", fmt.Errorf("object does not contain required marker"))
	}
	return Satisfied("object contains required marker")
}

func s3SatisfiedDetail(target S3Target, c *S3Condition) string {
	switch {
	case c.Contains != "":
		return "object contains required marker"
	case len(c.Metadata) > 0:
		return "object metadata matched"
	case target.Key != "":
		return "object exists"
	default:
		return "bucket exists"
	}
}

func signS3Request(req *http.Request, creds S3Credentials, region string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	shortDate := now.UTC().Format("20060102")
	scope := shortDate + "/" + region + "/s3/aws4_request"
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	canonicalRequest, signedHeaders := canonicalS3Request(req)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signingKey := s3SigningKey(creds.SecretAccessKey, shortDate, region)
	signature := hmacSHA256Hex(signingKey, stringToSign)
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKeyID,
		scope,
		signedHeaders,
		signature,
	))
}

func canonicalS3Request(req *http.Request) (string, string) {
	headers := s3SignedHeaders(req)
	lines := make([]string, 0, len(headers))
	names := make([]string, 0, len(headers))
	for _, name := range headers {
		names = append(names, name)
		lines = append(lines, name+":"+canonicalS3HeaderValue(req, name)+"\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonicalHeaders := strings.Join(lines, "")
	return strings.Join([]string{
		req.Method,
		canonicalS3Path(req.URL),
		canonicalS3Query(req.URL),
		canonicalHeaders,
		signedHeaders,
		req.Header.Get("X-Amz-Content-Sha256"),
	}, "\n"), signedHeaders
}

func s3SignedHeaders(req *http.Request) []string {
	headers := []string{"host"}
	for name := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "user-agent" {
			continue
		}
		headers = append(headers, lower)
	}
	sort.Strings(headers)
	return headers
}

func canonicalS3HeaderValue(req *http.Request, name string) string {
	if name == "host" {
		return req.URL.Host
	}
	values := req.Header.Values(name)
	for i, value := range values {
		values[i] = strings.Join(strings.Fields(value), " ")
	}
	return strings.Join(values, ",")
}

func canonicalS3Path(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func canonicalS3Query(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return u.RawQuery
	}
	return values.Encode()
}

func s3SigningKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hmacSHA256Hex(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, data))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
