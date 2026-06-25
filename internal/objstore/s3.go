package objstore

// S3Store is an S3-compatible SegmentStore (AWS S3, MinIO, Cloudflare R2,
// Backblaze B2, etc.) speaking plain net/http with AWS Signature V4 signing —
// no third-party SDK (zero-dependency invariant). Path-style addressing
// (endpoint/bucket/key) so it works with MinIO and most S3-compatibles.
//
// NOTE: the SigV4 signer below is unit-tested for structure/determinism and the
// client is tested end-to-end against a mock S3 over real HTTP, but interop with
// a live S3/MinIO should be smoke-tested before production use (per
// docs/object-store-backend.md).

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Creds holds the signing inputs. Service is "s3" for object stores.
type Creds struct {
	AccessKey string
	SecretKey string
	Region    string
	Service   string
}

// S3Store implements SegmentStore against an S3-compatible endpoint.
type S3Store struct {
	Endpoint string // e.g. https://s3.us-east-1.amazonaws.com or http://localhost:9000
	Bucket   string
	Creds    Creds
	Client   *http.Client
	nowFn    func() time.Time // overridable in tests

	gets, puts, heads   atomic.Int64
	getBytes, putBytes  atomic.Int64
}

// ObjStats reports access counts for cost visibility.
func (s *S3Store) ObjStats() ObjStats {
	return ObjStats{
		Gets: s.gets.Load(), Puts: s.puts.Load(), Heads: s.heads.Load(),
		GetBytes: s.getBytes.Load(), PutBytes: s.putBytes.Load(),
	}
}

func NewS3Store(endpoint, bucket string, c Creds) *S3Store {
	if c.Service == "" {
		c.Service = "s3"
	}
	return &S3Store{Endpoint: endpoint, Bucket: bucket, Creds: c}
}

func (s *S3Store) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s *S3Store) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

func (s *S3Store) url(key string) string {
	return strings.TrimRight(s.Endpoint, "/") + "/" + s.Bucket + "/" + strings.TrimLeft(key, "/")
}

func (s *S3Store) Put(key string, data []byte) error {
	req, err := http.NewRequest(http.MethodPut, s.url(key), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	signV4(req, data, s.Creds, s.now())
	s.puts.Add(1)
	s.putBytes.Add(int64(len(data)))
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("s3 put %s: %s: %s", key, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (s *S3Store) Get(key string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, s.url(key), nil)
	if err != nil {
		return nil, err
	}
	signV4(req, nil, s.Creds, s.now())
	s.gets.Add(1)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("s3 get %s: %s: %s", key, resp.Status, strings.TrimSpace(string(b)))
	}
	b, err := io.ReadAll(resp.Body)
	s.getBytes.Add(int64(len(b)))
	return b, err
}

func (s *S3Store) Exists(key string) (bool, error) {
	req, err := http.NewRequest(http.MethodHead, s.url(key), nil)
	if err != nil {
		return false, err
	}
	signV4(req, nil, s.Creds, s.now())
	s.heads.Add(1)
	resp, err := s.client().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode/100 == 2:
		return true, nil
	default:
		return false, fmt.Errorf("s3 head %s: %s", key, resp.Status)
	}
}

// ---- AWS Signature Version 4 (header auth) ----

func hmacSHA(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// aws4Encode percent-encodes per RFC 3986 (unreserved chars pass through). '/'
// is preserved in path position (encodeSlash=false).
func aws4Encode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// signV4 signs req in place (Authorization + X-Amz-Date + X-Amz-Content-Sha256).
// payload is the request body bytes (nil for empty). Requests must carry no query
// string (Centauri's object verbs don't use one).
func signV4(req *http.Request, payload []byte, c Creds, now time.Time) {
	t := now.UTC()
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")
	payloadHash := sha256hex(payload)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	host := req.URL.Host
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalURI := aws4Encode(req.URL.Path, false)
	canonicalRequest := strings.Join([]string{
		req.Method, canonicalURI, "", canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, c.Region, c.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA([]byte("AWS4"+c.SecretKey), []byte(dateStamp))
	kRegion := hmacSHA(kDate, []byte(c.Region))
	kService := hmacSHA(kRegion, []byte(c.Service))
	kSigning := hmacSHA(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA(kSigning, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKey, scope, signedHeaders, signature))
}
