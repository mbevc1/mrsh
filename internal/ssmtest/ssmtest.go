// Package ssmtest is an in-process fake of the SSM Parameter Store and STS
// API subset mrsh uses (PutParameter, GetParameters, DeleteParameter,
// AddTagsToResource, GetCallerIdentity). Tests point the real SDK at it
// through AWS_ENDPOINT_URL_SSM and AWS_ENDPOINT_URL_STS.
package ssmtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Account is the fake caller's account ID.
const Account = "123456789012"

// Param is a stored parameter.
type Param struct {
	Value   string
	Type    string
	Version int64
	Tags    map[string]string
}

// Server is a running fake.
type Server struct {
	URL    string
	Region string

	mu     sync.Mutex
	params map[string]*Param // by name
	calls  []string          // operation names, in order
}

// Start runs a fake until the test ends and points the AWS SDK at it.
func Start(t testing.TB) *Server {
	t.Helper()
	s := &Server{Region: "eu-west-1", params: map[string]*Param{}}
	hs := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(hs.Close)
	s.URL = hs.URL

	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_ENDPOINT_URL_SSM":        hs.URL,
		"AWS_ENDPOINT_URL_STS":        hs.URL,
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

// ARN is the ARN of the parameter name.
func (s *Server) ARN(name string) string {
	return fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s", s.Region, Account, name)
}

// Param returns a copy of a stored parameter, or nil.
func (s *Server) Param(name string) *Param {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.params[name]
	if !ok {
		return nil
	}
	cp := *p
	return &cp
}

// Put stores a parameter directly, as another writer would.
func (s *Server) Put(name, value string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.params[name] = &Param{Value: value, Type: "SecureString", Version: 1, Tags: map[string]string{}}
	return s.ARN(name)
}

// Calls returns the operations called so far.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)

	target := r.Header.Get("X-Amz-Target")
	if target == "" { // STS speaks the query protocol
		if !strings.Contains(string(body), "Action=GetCallerIdentity") {
			http.Error(w, "unsupported STS call", http.StatusBadRequest)
			return
		}
		s.calls = append(s.calls, "GetCallerIdentity")
		w.Header().Set("Content-Type", "text/xml")
		_, _ = fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>`+
			`<Arn>arn:aws:iam::%[1]s:user/test</Arn><UserId>AIDTEST</UserId><Account>%[1]s</Account>`+
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`, Account)
		return
	}

	op := strings.TrimPrefix(target, "AmazonSSM.")
	s.calls = append(s.calls, op)
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	str := func(k string) string { v, _ := in[k].(string); return v }

	switch op {
	case "PutParameter":
		name, overwrite := str("Name"), in["Overwrite"] == true
		tags, hasTags := in["Tags"].([]any)
		if hasTags && overwrite {
			fail(w, "ValidationException", "Invalid request: tags and overwrite can't be used together.")
			return
		}
		p, exists := s.params[name]
		if exists && !overwrite {
			fail(w, "ParameterAlreadyExists", "The parameter already exists.")
			return
		}
		if !exists {
			p = &Param{Tags: map[string]string{}}
			s.params[name] = p
		}
		p.Value, p.Type, p.Version = str("Value"), str("Type"), p.Version+1
		for _, tg := range tags {
			m, _ := tg.(map[string]any)
			p.Tags[fmt.Sprint(m["Key"])] = fmt.Sprint(m["Value"])
		}
		reply(w, map[string]any{"Version": p.Version, "Tier": "Standard"})

	case "GetParameters":
		names, _ := in["Names"].([]any)
		found, invalid := []map[string]any{}, []string{}
		for _, n := range names {
			req := fmt.Sprint(n)
			name := req
			if strings.HasPrefix(req, "arn:") {
				name = "/" + strings.TrimPrefix(strings.SplitN(req, ":parameter/", 2)[1], "/")
			}
			p, ok := s.params[name]
			if !ok {
				invalid = append(invalid, req)
				continue
			}
			arn := req
			if !strings.HasPrefix(req, "arn:") {
				arn = s.ARN(name)
			}
			found = append(found, map[string]any{"Name": name, "ARN": arn, "Value": p.Value, "Type": p.Type, "Version": p.Version})
		}
		reply(w, map[string]any{"Parameters": found, "InvalidParameters": invalid})

	case "DeleteParameter":
		if _, ok := s.params[str("Name")]; !ok {
			fail(w, "ParameterNotFound", "Parameter not found.")
			return
		}
		delete(s.params, str("Name"))
		reply(w, map[string]any{})

	case "AddTagsToResource":
		p, ok := s.params[str("ResourceId")]
		if !ok {
			fail(w, "InvalidResourceId", "The resource ID is not valid.")
			return
		}
		tags, _ := in["Tags"].([]any)
		for _, tg := range tags {
			m, _ := tg.(map[string]any)
			p.Tags[fmt.Sprint(m["Key"])] = fmt.Sprint(m["Value"])
		}
		reply(w, map[string]any{})

	default:
		fail(w, "UnsupportedOperation", op+" is not faked")
	}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": code, "message": msg})
}
