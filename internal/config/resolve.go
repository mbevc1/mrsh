package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// ssmBatchSize is the GetParameters limit per call.
const ssmBatchSize = 10

// AWSBackend fetches secret values from AWS. The real implementation is
// NewAWSBackend; tests supply a fake.
type AWSBackend interface {
	// GetParameters returns decrypted SSM parameter values keyed by the
	// requested ARN. At most ssmBatchSize ARNs are passed per call. ARNs
	// missing from the result are reported as not found.
	GetParameters(ctx context.Context, region string, arns []string) (map[string]string, error)
	// GetSecretValue returns the SecretString of a Secrets Manager secret.
	GetSecretValue(ctx context.Context, region, arn string) (string, error)
}

// Secrets are the resolved user and pass for one host. They never print
// their values through fmt or slog.
type Secrets struct {
	User string
	Pass string
}

// String redacts the password.
func (s Secrets) String() string {
	return fmt.Sprintf("{User:%s Pass:%s}", s.User, redacted(s.Pass))
}

// LogValue redacts the password in structured logs.
func (s Secrets) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user", s.User), slog.String("pass", redacted(s.Pass)))
}

func redacted(v string) string {
	if v == "" {
		return ""
	}
	return "[redacted]"
}

// Resolver turns user/pass references into values.
type Resolver struct {
	AWS    AWSBackend
	Getenv func(string) string
	Logger *slog.Logger
}

// NewResolver returns a Resolver using the process environment and a lazily
// configured AWS backend (standard SDK credential chain).
func NewResolver() *Resolver {
	return &Resolver{AWS: NewAWSBackend(), Getenv: os.Getenv}
}

// pending is one reference awaiting an AWS lookup.
type pending struct {
	host  int
	field string
	ref   SecretRef
	arn   arn
}

// Resolve resolves the user and pass references of hosts, which should be
// the effective (defaults-merged) targets only. The result is index-aligned
// with hosts. Every failure is reported; no error contains a secret value.
func (r *Resolver) Resolve(ctx context.Context, hosts []Host) ([]Secrets, error) {
	log := r.logger()
	out := make([]Secrets, len(hosts))
	var errs []error
	var aws []pending

	set := func(i int, field, v string) {
		if field == "user" {
			out[i].User = v
		} else {
			out[i].Pass = v
		}
	}

	for i, h := range hosts {
		for _, ref := range []SecretRef{h.UserRef(), h.PassRef()} {
			switch ref.Kind() {
			case KindPlain:
				set(i, ref.Field, ref.Literal)
				log.Debug("resolved secret", "host", h.Name, "field", ref.Field, "via", ref.Field, "backend", "plain", "bytes", len(ref.Literal))
			case KindEnv:
				v := r.getenv(ref.Env)
				if v == "" {
					errs = append(errs, fmt.Errorf("host %s: %s_env: environment variable %s is unset or empty", h.Name, ref.Field, ref.Env))
					log.Debug("resolve secret failed", "host", h.Name, "field", ref.Field, "via", ref.Field+"_env="+ref.Env, "backend", "env")
					continue
				}
				set(i, ref.Field, v)
				log.Debug("resolved secret", "host", h.Name, "field", ref.Field, "via", ref.Field+"_env="+ref.Env, "backend", "env", "bytes", len(v))
			case KindARN:
				a, err := parseARN(ref.ARN)
				if err != nil {
					errs = append(errs, fmt.Errorf("host %s: %s_arn: %w", h.Name, ref.Field, err))
					continue
				}
				aws = append(aws, pending{host: i, field: ref.Field, ref: ref, arn: a})
			}
		}
	}

	if len(aws) > 0 {
		if r.AWS == nil {
			errs = append(errs, errors.New("resolve secrets: *_arn references need an AWS backend"))
		} else {
			errs = append(errs, r.resolveSSM(ctx, hosts, aws, set)...)
			errs = append(errs, r.resolveSecretsManager(ctx, hosts, aws, set)...)
		}
	}
	return out, errors.Join(errs...)
}

func (r *Resolver) resolveSSM(ctx context.Context, hosts []Host, refs []pending, set func(int, string, string)) []error {
	log := r.logger()
	byRegion := map[string][]string{}
	seen := map[string]bool{}
	for _, p := range refs {
		if p.arn.Service != "ssm" || seen[p.arn.Base] {
			continue
		}
		seen[p.arn.Base] = true
		byRegion[p.arn.Region] = append(byRegion[p.arn.Region], p.arn.Base)
	}

	values := map[string]string{}
	failed := map[string]error{}
	for _, region := range sortedKeys(byRegion) {
		arns := byRegion[region]
		for start := 0; start < len(arns); start += ssmBatchSize {
			batch := arns[start:min(start+ssmBatchSize, len(arns))]
			t0 := time.Now()
			got, err := r.AWS.GetParameters(ctx, region, batch)
			log.Debug("ssm GetParameters", "region", region, "batch", len(batch), "ok", err == nil, "latency", time.Since(t0))
			for _, a := range batch {
				switch v, ok := got[a]; {
				case err != nil:
					failed[a] = err
				case !ok:
					failed[a] = errors.New("parameter not found")
				default:
					values[a] = v
				}
			}
		}
	}

	var errs []error
	for _, p := range refs {
		if p.arn.Service != "ssm" {
			continue
		}
		name := hosts[p.host].Name
		if err := failed[p.arn.Base]; err != nil {
			errs = append(errs, fmt.Errorf("host %s: %s_arn %s: %w", name, p.field, p.ref.ARN, err))
			continue
		}
		v := values[p.arn.Base]
		set(p.host, p.field, v)
		log.Debug("resolved secret", "host", name, "field", p.field, "via", p.field+"_arn", "backend", "ssm", "bytes", len(v))
	}
	return errs
}

func (r *Resolver) resolveSecretsManager(ctx context.Context, hosts []Host, refs []pending, set func(int, string, string)) []error {
	log := r.logger()
	type entry struct {
		value string
		err   error
	}
	cache := map[string]entry{}

	var errs []error
	for _, p := range refs {
		if p.arn.Service != "secretsmanager" {
			continue
		}
		name := hosts[p.host].Name
		e, hit := cache[p.arn.Base]
		if !hit {
			t0 := time.Now()
			v, err := r.AWS.GetSecretValue(ctx, p.arn.Region, p.arn.Base)
			e = entry{value: v, err: err}
			cache[p.arn.Base] = e
			log.Debug("secretsmanager GetSecretValue", "region", p.arn.Region, "cache", "miss", "ok", err == nil, "latency", time.Since(t0))
		} else {
			log.Debug("secretsmanager GetSecretValue", "region", p.arn.Region, "cache", "hit")
		}
		if e.err != nil {
			errs = append(errs, fmt.Errorf("host %s: %s_arn %s: %w", name, p.field, p.ref.ARN, e.err))
			continue
		}
		v := e.value
		if p.arn.Field != "" {
			var err error
			if v, err = jsonField(e.value, p.arn.Field); err != nil {
				errs = append(errs, fmt.Errorf("host %s: %s_arn %s: %w", name, p.field, p.ref.ARN, err))
				continue
			}
		}
		set(p.host, p.field, v)
		log.Debug("resolved secret", "host", name, "field", p.field, "via", p.field+"_arn", "backend", "secretsmanager", "json_field", p.arn.Field != "", "bytes", len(v))
	}
	return errs
}

// jsonField extracts one key from a JSON-object secret. Errors never quote
// the secret body.
func jsonField(secret, field string) (string, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(secret), &m); err != nil {
		return "", fmt.Errorf("secret is not a JSON object, cannot extract #%s", field)
	}
	v, ok := m[field]
	if !ok {
		return "", fmt.Errorf("secret has no JSON key %q", field)
	}
	switch v := v.(type) {
	case string:
		return v, nil
	case float64, bool:
		return fmt.Sprint(v), nil
	}
	return "", fmt.Errorf("secret JSON key %q is not a string, number or bool", field)
}

func (r *Resolver) getenv(k string) string {
	if r.Getenv != nil {
		return r.Getenv(k)
	}
	return os.Getenv(k)
}

func (r *Resolver) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// awsBackend is the AWS SDK v2 implementation of AWSBackend, with one client
// per region (the region comes from each ARN).
type awsBackend struct {
	mu     sync.Mutex
	cfg    *aws.Config
	ssm    map[string]*ssm.Client
	sm     map[string]*secretsmanager.Client
	loadFn func(ctx context.Context) (aws.Config, error)
}

// NewAWSBackend returns an AWSBackend that loads the default AWS credential
// chain on first use.
func NewAWSBackend() AWSBackend {
	return &awsBackend{
		ssm: map[string]*ssm.Client{},
		sm:  map[string]*secretsmanager.Client{},
		loadFn: func(ctx context.Context) (aws.Config, error) {
			return awsconfig.LoadDefaultConfig(ctx)
		},
	}
}

func (b *awsBackend) config(ctx context.Context) (aws.Config, error) {
	if b.cfg == nil {
		cfg, err := b.loadFn(ctx)
		if err != nil {
			return aws.Config{}, fmt.Errorf("load AWS config: %w", err)
		}
		b.cfg = &cfg
	}
	return *b.cfg, nil
}

func (b *awsBackend) ssmClient(ctx context.Context, region string) (*ssm.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.ssm[region]; ok {
		return c, nil
	}
	cfg, err := b.config(ctx)
	if err != nil {
		return nil, err
	}
	c := ssm.NewFromConfig(cfg, func(o *ssm.Options) { o.Region = region })
	b.ssm[region] = c
	return c, nil
}

func (b *awsBackend) smClient(ctx context.Context, region string) (*secretsmanager.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.sm[region]; ok {
		return c, nil
	}
	cfg, err := b.config(ctx)
	if err != nil {
		return nil, err
	}
	c := secretsmanager.NewFromConfig(cfg, func(o *secretsmanager.Options) { o.Region = region })
	b.sm[region] = c
	return c, nil
}

func (b *awsBackend) GetParameters(ctx context.Context, region string, arns []string) (map[string]string, error) {
	c, err := b.ssmClient(ctx, region)
	if err != nil {
		return nil, err
	}
	res, err := c.GetParameters(ctx, &ssm.GetParametersInput{Names: arns, WithDecryption: aws.Bool(true)})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(res.Parameters))
	for _, p := range res.Parameters {
		out[aws.ToString(p.ARN)] = aws.ToString(p.Value)
	}
	return out, nil
}

func (b *awsBackend) GetSecretValue(ctx context.Context, region, arn string) (string, error) {
	c, err := b.smClient(ctx, region)
	if err != nil {
		return "", err
	}
	res, err := c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
	if err != nil {
		return "", err
	}
	if res.SecretString != nil {
		return *res.SecretString, nil
	}
	return string(res.SecretBinary), nil
}
