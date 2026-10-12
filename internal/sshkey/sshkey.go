// Package sshkey reads SSH public keys and computes the fingerprints
// providers identify registered keys by.
package sshkey

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Key is one SSH public key.
type Key struct {
	// Type is the key type, e.g. ssh-ed25519.
	Type string
	// Blob is the decoded key material.
	Blob []byte
	// Comment is the trailing comment of the authorized_keys line.
	Comment string
}

// Parse reads one authorized_keys line.
func Parse(line string) (*Key, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return nil, errors.New("not an SSH public key (expected: type base64-blob [comment])")
	}
	if strings.HasPrefix(fields[0], "#") {
		return nil, errors.New("not an SSH public key (a comment line)")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) < 4 {
		return nil, errors.New("not an SSH public key (the key material is not base64)")
	}
	k := &Key{Type: fields[0], Blob: blob}
	if len(fields) > 2 {
		k.Comment = strings.Join(fields[2:], " ")
	}
	return k, nil
}

// ReadFile reads the first public key of a file, such as ~/.ssh/id_ed25519.pub.
func ReadFile(path string) (*Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, err := Parse(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return k, nil
	}
	return nil, fmt.Errorf("%s: no public key found", path)
}

// Line renders the key as an authorized_keys line.
func (k *Key) Line() string {
	s := k.Type + " " + base64.StdEncoding.EncodeToString(k.Blob)
	if k.Comment != "" {
		s += " " + k.Comment
	}
	return s
}

// FingerprintMD5 returns the legacy colon-separated MD5 fingerprint, the
// form Hetzner and OVH use to identify keys.
func (k *Key) FingerprintMD5() string {
	sum := md5.Sum(k.Blob)
	h := hex.EncodeToString(sum[:])
	parts := make([]string, 0, len(h)/2)
	for i := 0; i+2 <= len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// FingerprintSHA256 returns the fingerprint as ssh-keygen shows it.
func (k *Key) FingerprintSHA256() string {
	sum := sha256.Sum256(k.Blob)
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

// DefaultPaths are the public key files tried when none is given, in
// order of preference.
func DefaultPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		out = append(out, filepath.Join(home, ".ssh", n))
	}
	return out
}
