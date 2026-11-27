// Package ssl2 implements the legacy RC4-MD5 transport required by the PS2 FESL client.
package ssl2

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"time"
)

const maxRecord = 32767

var cipherSuite = []byte{1, 0, 0x80}
var errHandshake = errors.New("invalid SSLv2 handshake")
var errRecord = errors.New("invalid SSLv2 record")

type Server struct {
	cert []byte
	key  *rsa.PrivateKey
}

func Load(certFile, keyFile string) (*Server, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" || len(certBlock.Bytes)+30 > maxRecord {
		return nil, errors.New("SSLv2 requires a PEM certificate that fits one record")
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("SSLv2 requires a PEM RSA private key")
	}
	var key *rsa.PrivateKey
	if key, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if parseErr != nil {
			return nil, errors.New("invalid RSA private key")
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("SSLv2 requires an RSA private key")
		}
	}
	if key == nil {
		return nil, errors.New("SSLv2 requires an RSA key and a certificate that fits one record")
	}
	public, err := certificateRSAKey(certBlock.Bytes)
	if err != nil || key.Validate() != nil || public.E != key.PublicKey.E || public.N.Cmp(key.PublicKey.N) != 0 {
		return nil, errors.New("SSLv2 certificate and private key do not match")
	}
	return &Server{cert: certBlock.Bytes, key: key}, nil
}

func certificateRSAKey(der []byte) (*rsa.PublicKey, error) {
	var outer asn1.RawValue
	rest, err := asn1.Unmarshal(der, &outer)
	if err != nil || len(rest) != 0 || outer.Tag != asn1.TagSequence {
		return nil, errors.New("invalid certificate")
	}
	var tbs asn1.RawValue
	if _, err = asn1.Unmarshal(outer.Bytes, &tbs); err != nil || tbs.Tag != asn1.TagSequence {
		return nil, errors.New("invalid certificate body")
	}
	fields := tbs.Bytes
	var field asn1.RawValue
	fields, err = unmarshalASN1(fields, &field)
	if err != nil {
		return nil, err
	}
	if field.Class == asn1.ClassContextSpecific && field.Tag == 0 {
		fields, err = unmarshalASN1(fields, &field)
		if err != nil {
			return nil, err
		}
	}
	// Serial number is already consumed; skip signature, issuer, validity, and subject.
	for range 4 {
		fields, err = unmarshalASN1(fields, &field)
		if err != nil {
			return nil, err
		}
	}
	fields, err = unmarshalASN1(fields, &field)
	if err != nil || field.Tag != asn1.TagSequence {
		return nil, errors.New("certificate has no public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(field.FullBytes)
	if err != nil {
		return nil, err
	}
	public, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("SSLv2 requires an RSA certificate")
	}
	return public, nil
}

func unmarshalASN1(data []byte, value *asn1.RawValue) ([]byte, error) {
	rest, err := asn1.Unmarshal(data, value)
	return rest, err
}

func (s *Server) Accept(raw net.Conn) (net.Conn, error) {
	if err := raw.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return nil, err
	}
	stream := &stream{Conn: raw}
	hello, err := stream.readRecord()
	if err != nil {
		return nil, err
	}
	challenge := clientChallenge(hello)
	if challenge == nil {
		return nil, errHandshake
	}
	connID := make([]byte, 16)
	rand.Read(connID)
	reply := []byte{4, 0, 1, 0, 2}
	reply = binary.BigEndian.AppendUint16(reply, uint16(len(s.cert)))
	reply = append(reply, 0, 3, 0, 16)
	reply = append(reply, s.cert...)
	reply = append(reply, cipherSuite...)
	reply = append(reply, connID...)
	if err := stream.writeRecord(reply); err != nil {
		return nil, err
	}
	key, err := stream.readRecord()
	if err != nil {
		return nil, err
	}
	if len(key) != 10+s.key.Size() || key[0] != 2 || !bytes.Equal(key[1:4], cipherSuite) ||
		binary.BigEndian.Uint16(key[4:6]) != 0 || int(binary.BigEndian.Uint16(key[6:8])) != s.key.Size() ||
		binary.BigEndian.Uint16(key[8:10]) != 0 {
		return nil, errHandshake
	}
	// Keep a random key on invalid padding and reject at the encrypted finish.
	master := make([]byte, 16)
	rand.Read(master)
	if err := rsa.DecryptPKCS1v15SessionKey(rand.Reader, s.key, key[10:], master); err != nil {
		return nil, errHandshake
	}
	stream.out.enable(master, '0', challenge, connID)
	stream.in.enable(master, '1', challenge, connID)
	if err := stream.writeRecord(append([]byte{5}, challenge...)); err != nil {
		return nil, err
	}
	finished, err := stream.readRecord()
	if err != nil || !bytes.Equal(finished, append([]byte{3}, connID...)) {
		return nil, errHandshake
	}
	sessionID := make([]byte, 16)
	rand.Read(sessionID)
	if err := stream.writeRecord(append([]byte{6}, sessionID...)); err != nil {
		return nil, err
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return stream, nil
}

func clientChallenge(hello []byte) []byte {
	if len(hello) < 9 || hello[0] != 1 || binary.BigEndian.Uint16(hello[1:3]) != 2 {
		return nil
	}
	suites := int(binary.BigEndian.Uint16(hello[3:5]))
	session := int(binary.BigEndian.Uint16(hello[5:7]))
	challenge := int(binary.BigEndian.Uint16(hello[7:9]))
	if suites == 0 || suites%3 != 0 || (session != 0 && session != 16) ||
		challenge < 16 || challenge > 32 || len(hello) != 9+suites+session+challenge {
		return nil
	}
	for i := 9; i < 9+suites; i += 3 {
		if bytes.Equal(hello[i:i+3], cipherSuite) {
			return hello[len(hello)-challenge:]
		}
	}
	return nil
}

type direction struct {
	cipher *rc4.Cipher
	key    [md5.Size]byte
	seq    uint32
}

func (d *direction) enable(master []byte, digit byte, challenge, connID []byte) {
	key := append(append(append(bytes.Clone(master), digit), challenge...), connID...)
	d.key = md5.Sum(key)
	d.cipher, _ = rc4.NewCipher(d.key[:])
}

func (d *direction) mac(payload []byte) []byte {
	h := md5.New()
	h.Write(d.key[:])
	h.Write(payload)
	h.Write(binary.BigEndian.AppendUint32(nil, d.seq))
	return h.Sum(nil)
}

type stream struct {
	net.Conn
	in, out direction
	buffer  []byte
}

func (s *stream) readRecord() ([]byte, error) {
	var header [3]byte
	if _, err := io.ReadFull(s.Conn, header[:2]); err != nil {
		return nil, err
	}
	length, padding := int(binary.BigEndian.Uint16(header[:2])&0x7fff), 0
	if header[0]&0x80 == 0 {
		if header[0]&0x40 != 0 {
			return nil, errRecord
		}
		if _, err := io.ReadFull(s.Conn, header[2:]); err != nil {
			return nil, err
		}
		padding = int(header[2])
	}
	if length == 0 || (s.in.cipher == nil && padding != 0) ||
		(s.in.cipher != nil && length <= md5.Size+padding) {
		return nil, errRecord
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(s.Conn, data); err != nil {
		return nil, err
	}
	defer func() { s.in.seq++ }()
	if s.in.cipher == nil {
		return data, nil
	}
	s.in.cipher.XORKeyStream(data, data)
	if subtle.ConstantTimeCompare(data[:md5.Size], s.in.mac(data[md5.Size:])) != 1 {
		return nil, errRecord
	}
	return data[md5.Size : length-padding], nil
}

func (s *stream) writeRecord(payload []byte) error {
	data := payload
	if s.out.cipher != nil {
		data = append(s.out.mac(payload), payload...)
		s.out.cipher.XORKeyStream(data, data)
	}
	s.out.seq++
	record := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(data)), uint16(len(data))|0x8000)
	record = append(record, data...)
	n, err := s.Conn.Write(record)
	if err == nil && n != len(record) {
		return io.ErrShortWrite
	}
	return err
}

func (s *stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.buffer) == 0 {
		record, err := s.readRecord()
		if err != nil {
			return 0, err
		}
		s.buffer = record
	}
	n := copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	return n, nil
}

func (s *stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxRecord-md5.Size)
		if err := s.writeRecord(p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
