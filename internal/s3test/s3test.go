// Package s3test is an in-process fake of the S3 API subset mrsh uses
// (path-style GET/PUT object with conditional writes, multipart uploads,
// HEAD bucket). Tests point the real SDK at it through AWS_ENDPOINT_URL_S3.
package s3test

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Object is a stored object.
type Object struct {
	Data   []byte
	ETag   string
	SSE    string
	KMSKey string
}

// Request records one API call.
type Request struct {
	Method  string
	Op      string // PutObject, GetObject, CreateMultipartUpload, UploadPart, ...
	Key     string
	SSE     string
	KMSKey  string
	BodyLen int
	IfMatch string
	IfNone  string
}

// Server is a running fake.
type Server struct {
	URL    string
	Region string

	mu       sync.Mutex
	buckets  map[string]bool
	objects  map[string]*Object // "bucket/key"
	uploads  map[string]*upload
	requests []Request
	nextID   int
}

type upload struct {
	bucket, key, sse, kms string
	parts                 map[int][]byte
}

// Start runs a fake with the given buckets until the test ends, and points
// the AWS SDK at it through environment variables.
func Start(t testing.TB, buckets ...string) *Server {
	t.Helper()
	s := &Server{Region: "eu-west-1", buckets: map[string]bool{}, objects: map[string]*Object{}, uploads: map[string]*upload{}}
	for _, b := range buckets {
		s.buckets[b] = true
	}
	hs := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(hs.Close)
	s.URL = hs.URL

	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_ENDPOINT_URL_S3":         hs.URL,
		"AWS_ACCESS_KEY_ID":           "test",
		"AWS_SECRET_ACCESS_KEY":       "test",
		"AWS_REGION":                  s.Region,
		"AWS_CONFIG_FILE":             filepath.Join(dir, "config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_EC2_METADATA_DISABLED":   "true",
		"AWS_PROFILE":                 "",
	} {
		t.Setenv(k, v)
	}
	return s
}

// Object returns a stored object, or nil.
func (s *Server) Object(bucket, key string) *Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[bucket+"/"+key]
}

// Put stores an object directly, as another writer would.
func (s *Server) Put(bucket, key string, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := &Object{Data: data, ETag: etag(data)}
	s.objects[bucket+"/"+key] = o
	return o.ETag
}

// Requests returns the calls made so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func etag(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	body, err := readBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	req := Request{
		Method: r.Method, Key: key, BodyLen: len(body),
		SSE: r.Header.Get("X-Amz-Server-Side-Encryption"), KMSKey: r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"),
		IfMatch: r.Header.Get("If-Match"), IfNone: r.Header.Get("If-None-Match"),
	}
	defer func() { s.requests = append(s.requests, req) }()

	if !s.buckets[bucket] {
		req.Op = "NoSuchBucket"
		fail(w, http.StatusNotFound, "NoSuchBucket", "no such bucket")
		return
	}
	objKey := bucket + "/" + key

	switch {
	case r.Method == http.MethodHead && key == "":
		req.Op = "HeadBucket"
		w.Header().Set("X-Amz-Bucket-Region", s.Region)
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodGet:
		req.Op = "GetObject"
		o := s.objects[objKey]
		if o == nil {
			fail(w, http.StatusNotFound, "NoSuchKey", "no such key")
			return
		}
		w.Header().Set("ETag", o.ETag)
		w.Header().Set("Content-Length", strconv.Itoa(len(o.Data)))
		_, _ = w.Write(o.Data)

	case r.Method == http.MethodPost && q.Has("uploads"):
		req.Op = "CreateMultipartUpload"
		s.nextID++
		id := fmt.Sprintf("upload-%d", s.nextID)
		s.uploads[id] = &upload{bucket: bucket, key: key, sse: req.SSE, kms: req.KMSKey, parts: map[int][]byte{}}
		writeXML(w, fmt.Sprintf("<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", bucket, key, id))

	case r.Method == http.MethodPut && q.Has("uploadId"):
		req.Op = "UploadPart"
		u := s.uploads[q.Get("uploadId")]
		n, _ := strconv.Atoi(q.Get("partNumber"))
		if u == nil || n < 1 {
			fail(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		u.parts[n] = body
		w.Header().Set("ETag", etag(body))
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost && q.Has("uploadId"):
		req.Op = "CompleteMultipartUpload"
		u := s.uploads[q.Get("uploadId")]
		if u == nil {
			fail(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		nums := make([]int, 0, len(u.parts))
		for n := range u.parts {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		var data []byte
		for _, n := range nums {
			data = append(data, u.parts[n]...)
		}
		o := &Object{Data: data, ETag: etag(data), SSE: u.sse, KMSKey: u.kms}
		s.objects[objKey] = o
		delete(s.uploads, q.Get("uploadId"))
		writeXML(w, fmt.Sprintf("<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>", bucket, key, xmlEscape(o.ETag)))

	case r.Method == http.MethodDelete && q.Has("uploadId"):
		req.Op = "AbortMultipartUpload"
		delete(s.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPut:
		req.Op = "PutObject"
		cur := s.objects[objKey]
		if (req.IfNone == "*" && cur != nil) || (req.IfMatch != "" && (cur == nil || cur.ETag != req.IfMatch)) {
			fail(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
			return
		}
		o := &Object{Data: body, ETag: etag(body), SSE: req.SSE, KMSKey: req.KMSKey}
		s.objects[objKey] = o
		w.Header().Set("ETag", o.ETag)
		w.WriteHeader(http.StatusOK)

	default:
		fail(w, http.StatusNotImplemented, "NotImplemented", r.Method+" "+r.URL.String())
	}
}

// readBody reads the request body, decoding aws-chunked streaming uploads.
func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") &&
		!strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return raw, nil
	}
	br := bufio.NewReader(bytes.NewReader(raw))
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("aws-chunked: %w", err)
		}
		sizeHex, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		size, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("aws-chunked size %q: %w", line, err)
		}
		if size == 0 {
			return out, nil // trailers follow; ignore them
		}
		chunk := make([]byte, size+2) // data + CRLF
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, fmt.Errorf("aws-chunked data: %w", err)
		}
		out = append(out, chunk[:size]...)
	}
}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header+body)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "%s<Error><Code>%s</Code><Message>%s</Message></Error>", xml.Header, code, xmlEscape(msg))
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
