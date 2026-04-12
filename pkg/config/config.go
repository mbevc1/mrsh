package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/getsops/sops/v3/decrypt"
	"gopkg.in/yaml.v3"
)

// DefaultConfig holds default connection parameters applied to all hosts.
type DefaultConfig struct {
	User    string `yaml:"user"`
	Pass    string `yaml:"pass"`
	Port    int    `yaml:"port"`
	KeyFile string `yaml:"key_file"`
	Timeout int    `yaml:"timeout"`
}

// Host represents a single remote host entry.
type Host struct {
	Name    string `yaml:"name"`
	Address string `yaml:"host"`
	Group   string `yaml:"group,omitempty"`
	User    string `yaml:"user,omitempty"`
	Pass    string `yaml:"pass,omitempty"`
	Port    int    `yaml:"port,omitempty"`
	KeyFile string `yaml:"identity_file,omitempty"`
}

// Config is the top-level structure of hosts.yaml.
type Config struct {
	SOPS     map[string]interface{} `yaml:"sops,omitempty"`
	Defaults DefaultConfig          `yaml:"defaults"`
	Commands []string               `yaml:"commands,omitempty"`
	Hosts    []Host                 `yaml:"hosts"`
}

// secretsCache avoids duplicate AWS API calls within one mrsh invocation.
var secretsCache sync.Map

// Load reads and parses a hosts.yaml file. If the file contains a SOPS
// metadata block it is decrypted in-memory before parsing.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	// Detect SOPS-encrypted file by presence of the sops: top-level key.
	if bytes.Contains(raw, []byte("\nsops:")) || bytes.HasPrefix(raw, []byte("sops:")) {
		raw, err = decrypt.Data(raw, "yaml")
		if err != nil {
			return nil, fmt.Errorf("sops decrypt %s: %w", path, err)
		}
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := resolveHostSecrets(&cfg); err != nil {
		return nil, err
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

// applyDefaults fills zero-value host fields from cfg.Defaults.
func applyDefaults(cfg *Config) {
	d := cfg.Defaults
	for i := range cfg.Hosts {
		h := &cfg.Hosts[i]
		if h.User == "" {
			h.User = d.User
		}
		if h.Pass == "" {
			h.Pass = d.Pass
		}
		if h.Port == 0 {
			if d.Port != 0 {
				h.Port = d.Port
			} else {
				h.Port = 22
			}
		}
		if h.KeyFile == "" {
			h.KeyFile = d.KeyFile
		}
	}
}

// resolveHostSecrets expands secret references in each host's sensitive fields.
func resolveHostSecrets(cfg *Config) error {
	// Two-pass SSM batching: collect all SSM ARNs first.
	ssmARNs := collectSSMARNs(cfg)
	ssmValues, err := batchResolveSSM(ssmARNs)
	if err != nil {
		return err
	}

	for i := range cfg.Hosts {
		h := &cfg.Hosts[i]
		if h.User, err = resolveWithSSM(h.User, ssmValues); err != nil {
			return fmt.Errorf("host %s user: %w", h.Name, err)
		}
		if h.Pass, err = resolveWithSSM(h.Pass, ssmValues); err != nil {
			return fmt.Errorf("host %s pass: %w", h.Name, err)
		}
		if h.KeyFile, err = resolveWithSSM(h.KeyFile, ssmValues); err != nil {
			return fmt.Errorf("host %s key_file: %w", h.Name, err)
		}
	}
	return nil
}

// collectSSMARNs gathers all SSM parameter ARNs referenced across all hosts.
func collectSSMARNs(cfg *Config) []string {
	seen := map[string]bool{}
	var arns []string
	for _, h := range cfg.Hosts {
		for _, v := range []string{h.User, h.Pass, h.KeyFile} {
			if strings.HasPrefix(v, "arn:aws:ssm:") && !seen[v] {
				seen[v] = true
				arns = append(arns, v)
			}
		}
	}
	return arns
}

// batchResolveSSM fetches up to 10 SSM parameters per API call.
func batchResolveSSM(arns []string) (map[string]string, error) {
	if len(arns) == 0 {
		return nil, nil
	}

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	client := ssm.NewFromConfig(cfg)

	results := map[string]string{}
	// SSM GetParameters accepts max 10 names per call.
	for i := 0; i < len(arns); i += 10 {
		batch := arns[i:min(i+10, len(arns))]
		names := make([]string, len(batch))
		arnToName := map[string]string{}
		for j, arn := range batch {
			name := ssmNameFromARN(arn)
			names[j] = name
			arnToName[name] = arn
		}

		out, err := client.GetParameters(ctx, &ssm.GetParametersInput{
			Names:          names,
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			return nil, fmt.Errorf("ssm GetParameters: %w", err)
		}

		for _, p := range out.Parameters {
			if p.Name != nil && p.Value != nil {
				arn := arnToName[*p.Name]
				results[arn] = *p.Value
			}
		}
	}
	return results, nil
}

// ssmNameFromARN extracts the parameter name path from a SSM ARN.
// e.g. "arn:aws:ssm:eu-west-1:123:parameter/mrsh/web01/pass" → "/mrsh/web01/pass"
func ssmNameFromARN(arn string) string {
	const sep = ":parameter"
	idx := strings.Index(arn, sep)
	if idx < 0 {
		return arn
	}
	return arn[idx+len(sep):]
}

// resolveWithSSM resolves a value using a pre-fetched SSM map for SSM refs,
// and falls back to resolveValue for other types.
func resolveWithSSM(val string, ssmValues map[string]string) (string, error) {
	if strings.HasPrefix(val, "arn:aws:ssm:") {
		if v, ok := ssmValues[val]; ok {
			return v, nil
		}
		return "", fmt.Errorf("ssm parameter not found: %s", val)
	}
	return resolveValue(val)
}

// resolveValue resolves a single secret reference string.
func resolveValue(val string) (string, error) {
	switch {
	case val == "":
		return "", nil
	case strings.HasPrefix(val, "env:"):
		return os.Getenv(strings.TrimPrefix(val, "env:")), nil
	case strings.HasPrefix(val, "arn:aws:secretsmanager:"):
		return resolveSecretsManager(val)
	case strings.HasPrefix(val, "ENC["):
		// Pass-through: SOPS should have decrypted this already.
		// Returned as-is if SOPS decryption was skipped.
		return val, nil
	default:
		return val, nil
	}
}

// resolveSecretsManager fetches a value from AWS Secrets Manager.
// Supports an optional "#field" suffix to extract a specific JSON key.
func resolveSecretsManager(arnWithField string) (string, error) {
	arn := arnWithField
	field := ""
	if idx := strings.LastIndex(arnWithField, "#"); idx > 0 {
		arn = arnWithField[:idx]
		field = arnWithField[idx+1:]
	}

	// Check in-memory cache first.
	if cached, ok := secretsCache.Load(arn); ok {
		secret := cached.(string)
		return extractJSONField(secret, field)
	}

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("aws config: %w", err)
	}
	client := secretsmanager.NewFromConfig(cfg)

	out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(arn),
	})
	if err != nil {
		return "", fmt.Errorf("secretsmanager GetSecretValue %s: %w", arn, err)
	}

	secret := ""
	if out.SecretString != nil {
		secret = *out.SecretString
	}
	secretsCache.Store(arn, secret)
	return extractJSONField(secret, field)
}

// extractJSONField returns the named field from a JSON string, or the whole
// string if field is empty or the string is not valid JSON.
func extractJSONField(secret, field string) (string, error) {
	if field == "" {
		return secret, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(secret), &m); err != nil {
		return "", fmt.Errorf("secret is not a JSON object (needed field %q): %w", field, err)
	}
	v, ok := m[field]
	if !ok {
		return "", fmt.Errorf("field %q not found in secret", field)
	}
	return v, nil
}

// FilterHosts returns hosts matching the given group and/or name filters.
// If adhocHosts is non-empty, synthetic Host entries are created from
// comma-separated "user@host:port" strings and defaults are applied.
func FilterHosts(cfg *Config, group, host string, adhocHosts []string) []Host {
	if len(adhocHosts) > 0 {
		return parseAdhocHosts(adhocHosts, cfg.Defaults)
	}

	var out []Host
	for _, h := range cfg.Hosts {
		if group != "" && h.Group != group {
			continue
		}
		if host != "" && h.Name != host && h.Address != host {
			continue
		}
		out = append(out, h)
	}
	return out
}

// parseAdhocHosts creates Host entries from "user@host:port" strings.
func parseAdhocHosts(addrs []string, defaults DefaultConfig) []Host {
	hosts := make([]Host, 0, len(addrs))
	for _, addr := range addrs {
		h := Host{
			User:    defaults.User,
			Port:    defaults.Port,
			KeyFile: defaults.KeyFile,
		}
		if h.Port == 0 {
			h.Port = 22
		}

		// Parse user@host:port
		rest := addr
		if at := strings.Index(rest, "@"); at >= 0 {
			h.User = rest[:at]
			rest = rest[at+1:]
		}
		if colon := strings.LastIndex(rest, ":"); colon >= 0 {
			portStr := rest[colon+1:]
			h.Address = rest[:colon]
			port := 0
			fmt.Sscanf(portStr, "%d", &port)
			if port > 0 {
				h.Port = port
			}
		} else {
			h.Address = rest
		}
		h.Name = h.Address
		hosts = append(hosts, h)
	}
	return hosts
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RawConfig holds the loosely-typed YAML structure used for in-place mutation
// (add/update/remove hosts) without triggering secret resolution.
type RawConfig struct {
	Defaults map[string]interface{}   `yaml:"defaults,omitempty"`
	Commands []string                 `yaml:"commands,omitempty"`
	Hosts    []map[string]interface{} `yaml:"hosts"`
}

// ReadRaw reads the config file without secret resolution into a
// loosely-typed structure suitable for in-place modification.
func ReadRaw(path string) (*RawConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &RawConfig{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var rc RawConfig
	if err := yaml.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &rc, nil
}

// Write marshals rc and writes it to path.
func (rc *RawConfig) Write(path string) error {
	data, err := yaml.Marshal(rc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
