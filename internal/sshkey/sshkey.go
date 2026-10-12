// Package sshkey reads SSH public keys and computes the fingerprints
// providers identify registered keys by.
package sshkey

import (
	"crypto/ed25519"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
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

// Generate creates an ed25519 key pair: the private key in OpenSSH format
// at path (mode 0600, never overwriting an existing file) and the public
// key at path + ".pub".
func Generate(path, comment string) (*Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	if err := os.WriteFile(path+".pub", []byte(line+"\n"), 0o644); err != nil {
		os.Remove(path)
		return nil, err
	}
	return Parse(line)
}

// Ensure returns the public key of the key pair at path, generating the
// pair when the private key does not exist yet.
func Ensure(path, comment string) (key *Key, created bool, err error) {
	if _, err := os.Stat(path); err == nil {
		k, err := ReadFile(path + ".pub")
		if err != nil {
			return nil, false, fmt.Errorf("%s exists but its public key cannot be read: %w", path, err)
		}
		return k, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	k, err := Generate(path, comment)
	if err != nil {
		return nil, false, err
	}
	return k, true, nil
}

// DefaultComment names a generated key after the user and machine.
func DefaultComment() string {
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	switch {
	case user != "" && host != "":
		return "ump@" + user + "@" + host
	case host != "":
		return "ump@" + host
	}
	return "ump"
}

// DefaultIdentityFiles returns the user's standard private key files that
// exist, the ones ssh would try on its own. They are passed explicitly
// when another identity is added, because an explicit identity replaces
// ssh's defaults.
func DefaultIdentityFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_ed25519_sk", "id_ecdsa_sk"} {
		p := filepath.Join(home, ".ssh", n)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
