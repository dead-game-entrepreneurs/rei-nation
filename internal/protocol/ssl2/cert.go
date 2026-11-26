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

// GenerateCertificate builds the leaf certificate and key the PS2 FESL client
// accepts. The client's DirtySock SSL module (ProtoSSL) has a parser bug,
// documented at https://github.com/Aim4kill/Bug_OldProtoSSL, that skips
// signature verification when the outer certificate-signature algorithm is one
// it does not recognize. This builds an ordinary certificate chain, then
// rewrites the DER so the inner signature algorithm reads MD5-with-RSA and the
// outer one reads rsaEncryption, matching the layout that the retail client
// accepts. The certificate is otherwise not valid and must never be used off an
// isolated test network. The key is generated fresh and is not written to disk.
//
// The parser also requires both validity dates in ASN.1 UTCTime (year before
// 2050) and the subject and issuer names as PrintableString; Go emits both for
// the values used here.
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

// applyProtoSSLWorkaround rewrites the two signature-algorithm identifiers in a
// certificate DER. A certificate holds exactly two: the inner one in
// tbsCertificate and the outer one beside the signature. SHA1-with-RSA is used
// for both at signing time; the public-key OID in SubjectPublicKeyInfo is
// rsaEncryption and is left untouched because it does not match the search.
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
