package archive

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// s3UnsignedPayload lets the object body stream straight through instead of
// being buffered to hash it. S3 accepts it over HTTPS, and this project has no
// wish to pull an SDK in for one PUT.
const s3UnsignedPayload = "UNSIGNED-PAYLOAD"

// S3 writes archive objects to an S3-compatible bucket with a small SigV4
// signer rather than an SDK, matching how the rest of SoroBeacon avoids heavy
// dependencies. Credentials come from the environment
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY); the region comes from the URL's
// ?region= query, then AWS_REGION / AWS_DEFAULT_REGION, defaulting to
// us-east-1.
//
// Requests are virtual-hosted (bucket.s3.<region>.amazonaws.com) unless an
// ?endpoint= override is given, in which case path-style
// (<endpoint>/<bucket>/<key>) is used — that is what lets a MinIO or a test
// server stand in for S3.
type S3 struct {
	endpoint  string
	region    string
	bucket    string
	prefix    string
	access    string
	secret    string
	pathStyle bool
	http      *http.Client
}

func newS3(u *url.URL) (*S3, error) {
	bucket := u.Host
	if bucket == "" {
		return nil, errors.New("archive: s3 ARCHIVE_URL is missing a bucket (want s3://bucket/prefix)")
	}
	q := u.Query()
	region := q.Get("region")
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	access := os.Getenv("AWS_ACCESS_KEY_ID")
	secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if access == "" || secret == "" {
		return nil, errors.New("archive: s3 ARCHIVE_URL requires AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}

	s := &S3{
		region: region,
		bucket: bucket,
		prefix: strings.Trim(u.Path, "/"),
		access: access,
		secret: secret,
		http:   &http.Client{Timeout: 60 * time.Second},
	}
	if endpoint := q.Get("endpoint"); endpoint != "" {
		s.endpoint = strings.TrimRight(endpoint, "/")
		s.pathStyle = true
	} else {
		s.endpoint = "https://" + bucket + ".s3." + region + ".amazonaws.com"
	}
	return s, nil
}

// Archive PUTs the object. The key is uploaded under the configured prefix and
// overwrites any existing object of the same name, so a retried batch is
// idempotent.
func (s *S3) Archive(ctx context.Context, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	objectKey := key
	if s.prefix != "" {
		objectKey = s.prefix + "/" + key
	}
	rawURL := s.endpoint + "/" + objectKey
	if s.pathStyle {
		rawURL = s.endpoint + "/" + s.bucket + "/" + objectKey
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, r)
	if err != nil {
		return fmt.Errorf("archive: build s3 request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	s.sign(req, time.Now().UTC())

	res, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("archive: s3 put: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Read a bounded slice of the body for the error, then discard the
		// rest so a huge error page cannot be buffered.
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("archive: s3 put %s: status %d: %s", objectKey, res.StatusCode, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}

// sign adds the SigV4 headers for a single PUT. The payload is left unsigned
// (UNSIGNED-PAYLOAD), which is valid for S3 and lets the body stream.
func (s *S3) sign(req *http.Request, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("x-amz-content-sha256", s3UnsignedPayload)
	req.Header.Set("x-amz-date", amzDate)

	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + s3UnsignedPayload + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		s3UnsignedPayload,
	}, "\n")

	credentialScope := dateStamp + "/" + s.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hex.EncodeToString(sha256Bytes(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s.secret), dateStamp)
	kRegion := hmacSHA256(kDate, s.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.access+"/"+credentialScope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func sha256Bytes(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

var _ Archiver = (*S3)(nil)
var _ Archiver = (*Dir)(nil)
