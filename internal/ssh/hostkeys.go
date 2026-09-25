package ssh

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Host key policies.
const (
	PolicyStrict    = "strict"
	PolicyAcceptNew = "accept-new"
	PolicyInsecure  = "insecure"
)

// HostKeyChecker verifies host keys against known_hosts files. One checker
// is shared by every worker in a run so accept-new appends are serialized.
type HostKeyChecker struct {
	mu sync.Mutex
}

// ForPolicy returns a HostKeyVerifier for one policy and known_hosts path.
func (h *HostKeyChecker) ForPolicy(policy, knownHostsPath string) (HostKeyVerifier, error) {
	switch policy {
	case PolicyInsecure, "":
		return insecureVerifier{}, nil
	case PolicyStrict, PolicyAcceptNew:
		if knownHostsPath == "" {
			return nil, fmt.Errorf("host_key_policy %s needs a known_hosts path", policy)
		}
		return &knownHostsVerifier{checker: h, path: knownHostsPath, acceptNew: policy == PolicyAcceptNew}, nil
	}
	return nil, fmt.Errorf("unknown host_key_policy %q", policy)
}

type insecureVerifier struct{}

func (insecureVerifier) Verify(string) (gossh.HostKeyCallback, []string, error) {
	return gossh.InsecureIgnoreHostKey(), nil, nil
}

type knownHostsVerifier struct {
	checker   *HostKeyChecker
	path      string
	acceptNew bool
}

func (v *knownHostsVerifier) Verify(addr string) (gossh.HostKeyCallback, []string, error) {
	db, err := v.load()
	if err != nil {
		return nil, nil, err
	}
	algos := knownAlgorithms(db, addr)

	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		err := db(hostname, remote, key)
		var keyErr *knownhosts.KeyError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &keyErr) && len(keyErr.Want) > 0:
			return fmt.Errorf("host key for %s does not match %s (possible man-in-the-middle; remove the stale entry if the key changed legitimately)", hostname, v.path)
		case errors.As(err, &keyErr) && v.acceptNew:
			return v.checker.appendKey(v.path, hostname, remote, key)
		case errors.As(err, &keyErr):
			return fmt.Errorf("host %s is not in %s (add it with ssh-keyscan, or use --host-key-policy accept-new)", hostname, v.path)
		}
		return err
	}, algos, nil
}

// load parses the known_hosts file. accept-new creates a missing file.
func (v *knownHostsVerifier) load() (gossh.HostKeyCallback, error) {
	v.checker.mu.Lock()
	defer v.checker.mu.Unlock()
	if _, err := os.Stat(v.path); errors.Is(err, os.ErrNotExist) && v.acceptNew {
		if err := os.MkdirAll(filepath.Dir(v.path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(v.path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
	}
	db, err := knownhosts.New(v.path)
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	return db, nil
}

func (h *HostKeyChecker) appendKey(path, hostname string, remote net.Addr, key gossh.PublicKey) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Another worker may have added this host since our copy was loaded.
	if db, err := knownhosts.New(path); err == nil {
		if err := db(hostname, remote, key); err == nil {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("known_hosts: %w", err)
	}
	defer func() { _ = f.Close() }()
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	if _, err := fmt.Fprintln(f, line); err != nil {
		return fmt.Errorf("known_hosts: %w", err)
	}
	return nil
}

// knownAlgorithms returns the key algorithms known_hosts holds for addr, so
// the server is asked for a key type we can verify. It probes the database
// with a key no entry can match and reads the wanted keys from the error.
func knownAlgorithms(db gossh.HostKeyCallback, addr string) []string {
	err := db(addr, &net.TCPAddr{IP: net.IPv4zero}, probeKey{})
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) || len(keyErr.Want) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var algos []string
	for _, k := range keyErr.Want {
		for _, a := range algorithmsFor(k.Key.Type()) {
			if !seen[a] {
				seen[a] = true
				algos = append(algos, a)
			}
		}
	}
	return algos
}

// algorithmsFor maps a key type to the signature algorithms that use it.
func algorithmsFor(keyType string) []string {
	if keyType == gossh.KeyAlgoRSA {
		return []string{gossh.KeyAlgoRSASHA512, gossh.KeyAlgoRSASHA256, gossh.KeyAlgoRSA}
	}
	return []string{keyType}
}

// probeKey is a public key that matches no known_hosts entry.
type probeKey struct{}

func (probeKey) Type() string                          { return "mrsh-probe" }
func (probeKey) Marshal() []byte                       { return []byte("mrsh-probe") }
func (probeKey) Verify([]byte, *gossh.Signature) error { return errors.New("probe key") }
