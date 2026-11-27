package ssl2

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Signature-algorithm OID bodies (the bytes inside the OBJECT IDENTIFIER, not
// the tag or length). All three are nine bytes and differ only in the last.
var (
	oidSHA1WithRSA   = []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x05}
	oidMD5WithRSA    = []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x04}
	oidRSAEncryption = []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01}
)

// GenerateCertificate builds a throwaway certificate laid out for the PS2 ProtoSSL
// parser bug. See docs/games/gera/online-map.md#ssl-transport.
func GenerateCertificate(hostname string) (certDER []byte, key *rsa.PrivateKey, err error) {
	issuerKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, nil, err
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	notAfter := now.AddDate(10, 0, 0)
	if notAfter.Year() >= 2050 {
		return nil, nil, errors.New("ssl2: certificate lifetime must end before 2050")
	}
	issuerTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Country: []string{"US"}, Province: []string{"California"}, Locality: []string{"Redwood City"},
			Organization: []string{"Electronic Arts, Inc."}, OrganizationalUnit: []string{"Online Technology Group"},
			CommonName: "OTG3 Certificate Authority",
		},
		NotBefore: now.Add(-time.Hour), NotAfter: notAfter,
		SignatureAlgorithm:    x509.SHA1WithRSA,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTmpl, issuerTmpl, &issuerKey.PublicKey, issuerKey)
	if err != nil {
		return nil, nil, err
	}
	issuer, err := x509.ParseCertificate(issuerDER)
	if err != nil {
		return nil, nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Country: []string{"US"}, Province: []string{"California"},
			Organization: []string{"Electronic Arts, Inc."}, OrganizationalUnit: []string{"Global Online Studio"},
			CommonName: hostname,
		},
		NotBefore: now.Add(-time.Hour), NotAfter: notAfter,
		SignatureAlgorithm: x509.SHA1WithRSA,
		DNSNames:           []string{hostname},
		KeyUsage:           x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:        []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTmpl, issuer, &leafKey.PublicKey, issuerKey)
	if err != nil {
		return nil, nil, err
	}
	patched, err := applyProtoSSLWorkaround(der)
	if err != nil {
		return nil, nil, err
	}
	return patched, leafKey, nil
}

// applyProtoSSLWorkaround rewrites the inner signature OID to MD5-with-RSA and the
// outer one to rsaEncryption; the public-key OID does not match the search.
func applyProtoSSLWorkaround(der []byte) ([]byte, error) {
	var offsets []int
	for i := 0; ; {
		j := bytes.Index(der[i:], oidSHA1WithRSA)
		if j < 0 {
			break
		}
		offsets = append(offsets, i+j)
		i += j + len(oidSHA1WithRSA)
	}
	if len(offsets) != 2 {
		return nil, fmt.Errorf("ssl2: expected 2 signature-algorithm OIDs, found %d", len(offsets))
	}
	out := bytes.Clone(der)
	copy(out[offsets[0]:], oidMD5WithRSA)    // inner tbsCertificate.signature
	copy(out[offsets[1]:], oidRSAEncryption) // outer signatureAlgorithm
	return out, nil
}
