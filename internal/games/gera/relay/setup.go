package relay

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ValidNodeName limits names to characters safe in tokens.txt and file names.
func ValidNodeName(name string) bool {
	if name == "" || len(name) > 32 || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// tokens.txt has one line per node: "name sha256:<token hash> <public ip>".
// See docs/games/gera/internet-play.md#relay-directory.
const (
	CertFile   = "control-cert.pem"
	KeyFile    = "control-key.pem"
	TokensFile = "tokens.txt"
)

type tokenEntry struct {
	digest [32]byte
	ip     netip.Addr
}

func loadTokens(path string) (map[string]tokenEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tokens := map[string]tokenEntry{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		bad := fmt.Errorf("%s:%d: expected \"name sha256:<64 hex> <ipv4>\"", path, i+1)
		if len(parts) != 3 || !strings.HasPrefix(parts[1], "sha256:") {
			return nil, bad
		}
		raw, err := hex.DecodeString(parts[1][7:])
		ip, ipErr := netip.ParseAddr(parts[2])
		if err != nil || len(raw) != 32 || ipErr != nil || !ip.Is4() {
			return nil, bad
		}
		var e tokenEntry
		copy(e.digest[:], raw)
		e.ip = ip
		tokens[parts[0]] = e
	}
	return tokens, nil
}

func tokenMatches(token string, e tokenEntry) bool {
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], e.digest[:]) == 1
}

// serverTLS loads the control certificate for the server side of the link.
func serverTLS(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile))
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// pinnedClientConfig trusts exactly the given certificate.
func pinnedClientConfig(certPEM []byte) (*tls.Config, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("control certificate is not a PEM certificate")
	}
	pinned := block.Bytes
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The pinned certificate is the trust anchor; names are not checked.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || !bytes.Equal(raw[0], pinned) {
				return errors.New("server certificate does not match the pinned control certificate")
			}
			return nil
		},
	}, nil
}

// Setup creates the control certificate if needed and registers a node. It
// returns the node's token once; only its hash is stored.
func Setup(dir, name string, publicIP netip.Addr) (string, error) {
	if !ValidNodeName(name) {
		return "", errors.New("node name must be 1-32 ASCII letters, digits, '.', '_' or '-'")
	}
	if !publicIP.Is4() {
		return "", errors.New("node public IP must be IPv4")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	certPath, keyPath := filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile)
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		certPEM, keyPEM, err := newControlCert()
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			return "", err
		}
		if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
			return "", err
		}
	}
	tokensPath := filepath.Join(dir, TokensFile)
	if _, err := os.Stat(tokensPath); err == nil {
		if _, err := loadTokens(tokensPath); err != nil {
			return "", err
		}
	}
	var raw [32]byte
	rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(token))
	var lines []string
	if data, err := os.ReadFile(tokensPath); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if f := strings.Fields(line); len(f) > 0 && f[0] == name {
				continue // re-issuing replaces the old token
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
	}
	lines = append(lines, fmt.Sprintf("%s sha256:%s %s", name, hex.EncodeToString(sum[:]), publicIP))
	if err := os.WriteFile(tokensPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func newControlCert() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Rei Nation relay control"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
