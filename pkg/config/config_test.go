package config

import (
	"os"
	"testing"
)

// TestResolveValue tests the resolveValue function with all supported prefix types.
func TestResolveValue(t *testing.T) {
	os.Setenv("TEST_SECRET_VAR", "secretvalue")
	defer os.Unsetenv("TEST_SECRET_VAR")

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"empty string", "", "", false},
		{"plain string", "plaintext", "plaintext", false},
		{"plain with spaces", "hello world", "hello world", false},
		{"env prefix found", "env:TEST_SECRET_VAR", "secretvalue", false},
		{"env prefix missing var", "env:NONEXISTENT_VAR_12345", "", false},
		{"ENC passthrough", "ENC[AES256_GCM,data:abc,type:str]", "ENC[AES256_GCM,data:abc,type:str]", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveValue(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveValue(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("resolveValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestLoad tests loading and parsing a plain (non-SOPS) hosts.yaml.
func TestLoad(t *testing.T) {
	cfg, err := Load("testdata/hosts.yaml")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Hosts) != 2 {
		t.Fatalf("expected 2 hosts, got %d", len(cfg.Hosts))
	}

	// host1 has an explicit user; defaults.user should not overwrite it.
	h1 := cfg.Hosts[0]
	if h1.Name != "host1" {
		t.Errorf("host[0].Name = %q, want host1", h1.Name)
	}
	if h1.User != "deploy" {
		t.Errorf("host[0].User = %q, want deploy (explicit override)", h1.User)
	}
	if h1.Port != 22 {
		t.Errorf("host[0].Port = %d, want 22 (from defaults)", h1.Port)
	}

	// host2 has explicit port; defaults should fill user.
	h2 := cfg.Hosts[1]
	if h2.Port != 2222 {
		t.Errorf("host[1].Port = %d, want 2222", h2.Port)
	}
	if h2.User != "testuser" {
		t.Errorf("host[1].User = %q, want testuser (from defaults)", h2.User)
	}
}

// TestLoadMissingFile tests that a missing config file returns an empty Config.
func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load("/nonexistent/path/hosts.yaml")
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil Config for missing file")
	}
	if len(cfg.Hosts) != 0 {
		t.Errorf("expected 0 hosts for missing file, got %d", len(cfg.Hosts))
	}
}

// TestFilterHosts tests host filtering by group and name.
func TestFilterHosts(t *testing.T) {
	cfg, err := Load("testdata/hosts.yaml")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	t.Run("filter by group", func(t *testing.T) {
		hosts := FilterHosts(cfg, "web", "", nil)
		if len(hosts) != 1 {
			t.Errorf("expected 1 host in group web, got %d", len(hosts))
		}
		if hosts[0].Name != "host1" {
			t.Errorf("expected host1, got %s", hosts[0].Name)
		}
	})

	t.Run("filter by name", func(t *testing.T) {
		hosts := FilterHosts(cfg, "", "host2", nil)
		if len(hosts) != 1 {
			t.Errorf("expected 1 host named host2, got %d", len(hosts))
		}
	})

	t.Run("no filter returns all", func(t *testing.T) {
		hosts := FilterHosts(cfg, "", "", nil)
		if len(hosts) != 2 {
			t.Errorf("expected 2 hosts with no filter, got %d", len(hosts))
		}
	})

	t.Run("adhoc hosts", func(t *testing.T) {
		defaults := DefaultConfig{User: "root", Port: 22}
		cfg2 := &Config{Defaults: defaults}
		hosts := FilterHosts(cfg2, "", "", []string{"root@192.168.1.1:22", "10.0.0.5"})
		if len(hosts) != 2 {
			t.Errorf("expected 2 adhoc hosts, got %d", len(hosts))
		}
		if hosts[0].User != "root" {
			t.Errorf("expected user root, got %s", hosts[0].User)
		}
		if hosts[0].Address != "192.168.1.1" {
			t.Errorf("expected address 192.168.1.1, got %s", hosts[0].Address)
		}
	})
}

// TestSSMNameFromARN tests the SSM ARN → parameter name extraction.
func TestSSMNameFromARN(t *testing.T) {
	tests := []struct {
		arn  string
		want string
	}{
		{
			"arn:aws:ssm:eu-west-1:123456789:parameter/mrsh/web01/pass",
			"/mrsh/web01/pass",
		},
		{
			"arn:aws:ssm:us-east-1:999:parameter/prod/db/secret",
			"/prod/db/secret",
		},
	}
	for _, tt := range tests {
		got := ssmNameFromARN(tt.arn)
		if got != tt.want {
			t.Errorf("ssmNameFromARN(%q) = %q, want %q", tt.arn, got, tt.want)
		}
	}
}

// TestExtractJSONField tests JSON field extraction for Secrets Manager values.
func TestExtractJSONField(t *testing.T) {
	secret := `{"user":"admin","pass":"s3cr3t"}`

	val, err := extractJSONField(secret, "user")
	if err != nil || val != "admin" {
		t.Errorf("extractJSONField user: got %q, %v", val, err)
	}

	val, err = extractJSONField(secret, "pass")
	if err != nil || val != "s3cr3t" {
		t.Errorf("extractJSONField pass: got %q, %v", val, err)
	}

	// No field — return whole secret.
	val, err = extractJSONField(secret, "")
	if err != nil || val != secret {
		t.Errorf("extractJSONField empty field: got %q, %v", val, err)
	}

	// Missing field.
	_, err = extractJSONField(secret, "nonexistent")
	if err == nil {
		t.Error("expected error for missing field")
	}
}
