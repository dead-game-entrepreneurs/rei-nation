// Package ssl2test is an independent SSL 2.0 RC4-MD5 test client, so tests do not
// derive their expectations from the server's own code.
package ssl2test

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
)

var rc4MD5 = []byte{0x01, 0x00, 0x80}

const macSize = md5.Size

// Client is a connected SSL 2.0 stream. Read and Write carry records; the
// embedded net.Conn supplies deadlines and Close.
type Client struct {
	net.Conn
	readSeq, writeSeq uint32
	enc, dec          *rc4.Cipher
	readKey, writeKey []byte
	plain             []byte
}

// Dial performs the SSL 2.0 handshake over raw and returns the secured stream.
func Dial(raw net.Conn) (*Client, error) {
	c := &Client{Conn: raw}
	challenge := make([]byte, 16)
	rand.Read(challenge)
	hello := []byte{1}
	hello = u16(hello, 2)
	hello = u16(hello, len(rc4MD5))
	hello = u16(hello, 0)
	hello = u16(hello, len(challenge))
	hello = append(hello, rc4MD5...)
	hello = append(hello, challenge...)
	if err := c.writeRecord(hello); err != nil {
		return nil, err
	}
	serverHello, err := c.readRecord()
	if err != nil {
		return nil, err
	}
	if len(serverHello) < 11 || serverHello[0] != 4 {
		return nil, errors.New("ssl2test: expected SERVER-HELLO")
	}
	certLen := int(binary.BigEndian.Uint16(serverHello[5:7]))
	csLen := int(binary.BigEndian.Uint16(serverHello[7:9]))
	connLen := int(binary.BigEndian.Uint16(serverHello[9:11]))
	if len(serverHello) != 11+certLen+csLen+connLen {
		return nil, errors.New("ssl2test: malformed SERVER-HELLO")
	}
	certDER := serverHello[11 : 11+certLen]
	connID := serverHello[11+certLen+csLen:]
	pub, err := PublicKeyFromCertificate(certDER)
	if err != nil {
		return nil, err
	}

	master := make([]byte, 16)
	rand.Read(master)
	encMaster, err := rsa.EncryptPKCS1v15(rand.Reader, pub, master)
	if err != nil {
		return nil, err
	}
	masterKey := []byte{2}
	masterKey = append(masterKey, rc4MD5...)
	masterKey = u16(masterKey, 0)
	masterKey = u16(masterKey, len(encMaster))
	masterKey = u16(masterKey, 0)
	masterKey = append(masterKey, encMaster...)
	if err := c.writeRecord(masterKey); err != nil {
		return nil, err
	}
	// The client reads with the server's write key and writes with its read key.
	c.readKey = keyedMD5(master, []byte("0"), challenge, connID)
	c.writeKey = keyedMD5(master, []byte("1"), challenge, connID)
	c.enc, _ = rc4.NewCipher(c.writeKey)
	c.dec, _ = rc4.NewCipher(c.readKey)

	verify, err := c.readRecord()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(verify, append([]byte{5}, challenge...)) {
		return nil, errors.New("ssl2test: SERVER-VERIFY did not echo the challenge")
	}
	if err := c.writeRecord(append([]byte{3}, connID...)); err != nil {
		return nil, err
	}
	finished, err := c.readRecord()
	if err != nil {
		return nil, err
	}
	if len(finished) != 17 || finished[0] != 6 {
		return nil, errors.New("ssl2test: unexpected SERVER-FINISHED")
	}
	return c, nil
}

func (c *Client) Read(p []byte) (int, error) {
	for len(c.plain) == 0 {
		rec, err := c.readRecord()
		if err != nil {
			return 0, err
		}
		c.plain = rec
	}
	n := copy(p, c.plain)
	c.plain = c.plain[n:]
	return n, nil
}

func (c *Client) Write(p []byte) (int, error) {
	if err := c.writeRecord(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *Client) writeRecord(payload []byte) error {
	seq := c.writeSeq
	c.writeSeq++
	body := payload
	if c.enc != nil {
		body = make([]byte, macSize+len(payload))
		copy(body, recordMAC(c.writeKey, payload, seq))
		copy(body[macSize:], payload)
		c.enc.XORKeyStream(body, body)
	}
	rec := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(rec[:2], 0x8000|uint16(len(body)))
	copy(rec[2:], body)
	_, err := c.Conn.Write(rec)
	return err
}

func (c *Client) readRecord() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.Conn, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0]&0x80 == 0 {
		return nil, errors.New("ssl2test: expected a two-byte record header")
	}
	length := int(hdr[0]&0x7f)<<8 | int(hdr[1])
	data := make([]byte, length)
	if _, err := io.ReadFull(c.Conn, data); err != nil {
		return nil, err
	}
	seq := c.readSeq
	c.readSeq++
	if c.dec == nil {
		return data, nil
	}
	c.dec.XORKeyStream(data, data)
	if len(data) < macSize {
		return nil, errors.New("ssl2test: short encrypted record")
	}
	mac, content := data[:macSize], data[macSize:]
	if !bytes.Equal(mac, recordMAC(c.readKey, content, seq)) {
		return nil, errors.New("ssl2test: record MAC mismatch")
	}
	return content, nil
}

// PublicKeyFromCertificate reads the RSA key from SubjectPublicKeyInfo, because
// crypto/x509 rejects the deliberately mismatched signature OIDs.
func PublicKeyFromCertificate(der []byte) (*rsa.PublicKey, error) {
	var cert struct {
		Raw asn1.RawContent
		TBS struct {
			Raw          asn1.RawContent
			Version      int `asn1:"optional,explicit,default:0,tag:0"`
			SerialNumber *big.Int
			SigAlg       asn1.RawValue
			Issuer       asn1.RawValue
			Validity     asn1.RawValue
			Subject      asn1.RawValue
			PublicKey    asn1.RawValue
		}
		SigAlg asn1.RawValue
		Sig    asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &cert); err != nil {
		return nil, fmt.Errorf("ssl2test: parse certificate: %w", err)
	}
	key, err := x509.ParsePKIXPublicKey(cert.TBS.PublicKey.FullBytes)
	if err != nil {
		return nil, fmt.Errorf("ssl2test: parse public key: %w", err)
	}
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("ssl2test: certificate key is not RSA")
	}
	return pub, nil
}

func recordMAC(secret, content []byte, seq uint32) []byte {
	var seqBuf [4]byte
	binary.BigEndian.PutUint32(seqBuf[:], seq)
	return keyedMD5(secret, content, seqBuf[:])
}

func keyedMD5(parts ...[]byte) []byte {
	h := md5.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func u16(b []byte, v int) []byte { return append(b, byte(v>>8), byte(v)) }
