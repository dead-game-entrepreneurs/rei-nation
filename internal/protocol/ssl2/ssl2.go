// Package ssl2 implements the obsolete SSL 2.0 RC4-128-MD5 transport that PS2 FESL
// clients require. See docs/games/gera/online-map.md#ssl-transport.
package ssl2

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// rc4MD5 is the SSL 2.0 cipher spec SSL_CK_RC4_128_WITH_MD5.
var rc4MD5 = []byte{0x01, 0x00, 0x80}

const (
	maxRecord        = 32767
	macSize          = md5.Size
	handshakeTimeout = 20 * time.Second
)

// NewListener wraps a listener so each connection negotiates SSL 2.0 before any
// application data. log may be nil; when set, handshake steps are logged.
func NewListener(inner net.Listener, certDER []byte, key *rsa.PrivateKey, log *slog.Logger) net.Listener {
	return &listener{Listener: inner, cert: certDER, key: key, log: log}
}

type listener struct {
	net.Listener
	cert []byte
	key  *rsa.PrivateKey
	log  *slog.Logger
}

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	// Keepalive on the raw socket, since the SSL wrapper hides the TCP conn from
	// the server loop and a login connection may idle through a level load.
	if ka, ok := c.(interface {
		SetKeepAlive(bool) error
		SetKeepAlivePeriod(time.Duration) error
	}); ok {
		ka.SetKeepAlive(true)
		ka.SetKeepAlivePeriod(30 * time.Second)
	}
	if l.log != nil {
		l.log.Info("FESL TCP connection accepted", "remote", c.RemoteAddr().String())
	}
	return &Conn{Conn: c, cert: l.cert, key: l.key, log: l.log}, nil
}

// Conn carries SSL 2.0 records. The handshake runs on the first Read or Write so
// Accept never blocks; afterwards one reader and one writer may run concurrently.
type Conn struct {
	net.Conn
	cert []byte
	key  *rsa.PrivateKey
	log  *slog.Logger

	hsOnce sync.Once
	hsErr  error

	// The caller's deadlines, restored after the handshake replaces them.
	deadlineMu    sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time

	readMu   sync.Mutex
	writeMu  sync.Mutex
	readSeq  uint32
	writeSeq uint32
	readKey  []byte
	writeKey []byte
	enc      *rc4.Cipher
	dec      *rc4.Cipher
	plain    []byte // decrypted bytes not yet returned by Read
}

// SetReadDeadline records the caller's deadline so the handshake can restore it.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDeadline = t
	return c.Conn.SetReadDeadline(t)
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.writeDeadline = t
	return c.Conn.SetWriteDeadline(t)
}

func (c *Conn) SetDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDeadline, c.writeDeadline = t, t
	return c.Conn.SetDeadline(t)
}

func (c *Conn) handshake() error {
	c.hsOnce.Do(func() { c.hsErr = c.doHandshake() })
	return c.hsErr
}

func (c *Conn) Read(p []byte) (int, error) {
	if err := c.handshake(); err != nil {
		return 0, err
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
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

func (c *Conn) Write(p []byte) (int, error) {
	if err := c.handshake(); err != nil {
		return 0, err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeRecords(p)
}

// readRecord reads one record, decrypting and checking its MAC once encryption is
// on. Callers hold readMu or run inside the handshake.
func (c *Conn) readRecord() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.Conn, hdr[:]); err != nil {
		return nil, err
	}
	var length, padLen int
	if hdr[0]&0x80 != 0 {
		length = int(hdr[0]&0x7f)<<8 | int(hdr[1])
	} else {
		if hdr[0]&0x40 != 0 {
			return nil, errors.New("ssl2: security escape records are unsupported")
		}
		length = int(hdr[0]&0x3f)<<8 | int(hdr[1])
		var pad [1]byte
		if _, err := io.ReadFull(c.Conn, pad[:]); err != nil {
			return nil, err
		}
		padLen = int(pad[0])
	}
	if length <= 0 || length > maxRecord || padLen > length {
		return nil, errors.New("ssl2: invalid record length or padding")
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(c.Conn, data); err != nil {
		return nil, err
	}
	seq := c.readSeq
	c.readSeq++
	if c.dec == nil {
		if padLen != 0 {
			return nil, errors.New("ssl2: padding on a cleartext record")
		}
		return data, nil
	}
	c.dec.XORKeyStream(data, data)
	if len(data) < macSize+padLen {
		return nil, errors.New("ssl2: encrypted record too short")
	}
	mac, content := data[:macSize], data[macSize:]
	if subtle.ConstantTimeCompare(mac, recordMAC(c.readKey, content, seq)) != 1 {
		return nil, errors.New("ssl2: record MAC mismatch")
	}
	if padLen != 0 {
		content = content[:len(content)-padLen]
	}
	return content, nil
}

// writeRecords splits payload into unpadded records and returns the payload bytes
// written; RC4 is a stream cipher, so no padding is needed.
func (c *Conn) writeRecords(payload []byte) (int, error) {
	max := maxRecord
	if c.enc != nil {
		max -= macSize
	}
	for start := 0; start < len(payload); start += max {
		end := min(start+max, len(payload))
		chunk := payload[start:end]
		seq := c.writeSeq
		c.writeSeq++
		var body []byte
		if c.enc != nil {
			body = make([]byte, macSize+len(chunk))
			copy(body, recordMAC(c.writeKey, chunk, seq))
			copy(body[macSize:], chunk)
			c.enc.XORKeyStream(body, body)
		} else {
			body = chunk
		}
		record := make([]byte, 2+len(body))
		binary.BigEndian.PutUint16(record[:2], 0x8000|uint16(len(body)))
		copy(record[2:], body)
		if _, err := c.Conn.Write(record); err != nil {
			return start, err
		}
	}
	return len(payload), nil
}

func (c *Conn) doHandshake() (err error) {
	c.Conn.SetDeadline(time.Now().Add(handshakeTimeout))
	c.trace("SSL handshake started")
	defer func() {
		if err != nil {
			c.trace("SSL handshake failed", "error", err.Error())
		}
	}()

	rec, err := c.readRecord()
	if err != nil {
		return err
	}
	challenge, err := parseClientHello(rec)
	if err != nil {
		return err
	}
	c.trace("SSL ClientHello received", "bytes", len(rec))

	connID := make([]byte, 16)
	if _, err := rand.Read(connID); err != nil {
		return err
	}
	hello := make([]byte, 0, 11+len(c.cert)+len(rc4MD5)+len(connID))
	hello = append(hello, 4, 0, 1) // SERVER-HELLO, no session hit, X.509 certificate
	hello = appendU16(hello, 2)    // server SSL version
	hello = appendU16(hello, len(c.cert))
	hello = appendU16(hello, len(rc4MD5))
	hello = appendU16(hello, len(connID))
	hello = append(hello, c.cert...)
	hello = append(hello, rc4MD5...)
	hello = append(hello, connID...)
	if _, err := c.writeRecords(hello); err != nil {
		return err
	}
	c.trace("SSL ServerHello sent", "cert_bytes", len(c.cert))

	rec, err = c.readRecord()
	if err != nil {
		return err
	}
	master, err := parseMasterKey(rec, c.key)
	if err != nil {
		return err
	}
	c.enableEncryption(master, challenge, connID)
	c.trace("SSL ClientMasterKey accepted")

	if _, err := c.writeRecords(append([]byte{5}, challenge...)); err != nil { // SERVER-VERIFY
		return err
	}
	rec, err = c.readRecord()
	if err != nil {
		return err
	}
	if !bytes.Equal(rec, append([]byte{3}, connID...)) { // CLIENT-FINISHED
		return errors.New("ssl2: client finished connection ID mismatch")
	}
	sessionID := make([]byte, 16)
	if _, err := rand.Read(sessionID); err != nil {
		return err
	}
	if _, err := c.writeRecords(append([]byte{6}, sessionID...)); err != nil { // SERVER-FINISHED
		return err
	}

	c.deadlineMu.Lock()
	c.Conn.SetReadDeadline(c.readDeadline)
	c.Conn.SetWriteDeadline(c.writeDeadline)
	c.deadlineMu.Unlock()
	c.trace("SSL handshake complete")
	return nil
}

func (c *Conn) trace(msg string, args ...any) {
	if c.log != nil {
		c.log.Info(msg, append([]any{"remote", c.Conn.RemoteAddr().String()}, args...)...)
	}
}

// enableEncryption derives the directional keys. The server writes with the "0"
// key and reads with the "1" key; the client mirrors them.
func (c *Conn) enableEncryption(master, challenge, connID []byte) {
	c.writeKey = keyedMD5(master, []byte("0"), challenge, connID)
	c.readKey = keyedMD5(master, []byte("1"), challenge, connID)
	// rc4.NewCipher only fails on an out-of-range key length; these are 16 bytes.
	c.enc, _ = rc4.NewCipher(c.writeKey)
	c.dec, _ = rc4.NewCipher(c.readKey)
}

func parseClientHello(d []byte) (challenge []byte, err error) {
	if len(d) < 9 || d[0] != 1 {
		return nil, errors.New("ssl2: expected a ClientHello")
	}
	version := binary.BigEndian.Uint16(d[1:3])
	csLen := int(binary.BigEndian.Uint16(d[3:5]))
	sidLen := int(binary.BigEndian.Uint16(d[5:7]))
	chLen := int(binary.BigEndian.Uint16(d[7:9]))
	if version != 2 || csLen == 0 || csLen%3 != 0 || (sidLen != 0 && sidLen != 16) {
		return nil, errors.New("ssl2: unsupported ClientHello version, cipher list, or session ID")
	}
	if chLen < 16 || chLen > 32 || len(d) != 9+csLen+sidLen+chLen {
		return nil, errors.New("ssl2: invalid ClientHello lengths")
	}
	ciphers := d[9 : 9+csLen]
	offered := false
	for i := 0; i < csLen; i += 3 {
		if bytes.Equal(ciphers[i:i+3], rc4MD5) {
			offered = true
			break
		}
	}
	if !offered {
		return nil, errors.New("ssl2: client did not offer RC4-128-MD5")
	}
	return d[len(d)-chLen:], nil
}

func parseMasterKey(d []byte, key *rsa.PrivateKey) ([]byte, error) {
	if len(d) > 0 && d[0] == 0 {
		var code uint16
		if len(d) >= 3 {
			code = binary.BigEndian.Uint16(d[1:3])
		}
		return nil, fmt.Errorf("ssl2: client sent error %d after the certificate", code)
	}
	if len(d) < 10 || d[0] != 2 || !bytes.Equal(d[1:4], rc4MD5) {
		return nil, errors.New("ssl2: expected an RC4 ClientMasterKey")
	}
	clearLen := binary.BigEndian.Uint16(d[4:6])
	encLen := int(binary.BigEndian.Uint16(d[6:8]))
	argLen := binary.BigEndian.Uint16(d[8:10])
	if clearLen != 0 || argLen != 0 || encLen == 0 || len(d) != 10+encLen {
		return nil, errors.New("ssl2: invalid ClientMasterKey lengths")
	}
	// A bad padding keeps the random key, so the failure surfaces only at
	// CLIENT-FINISHED and does not reveal padding validity.
	master := make([]byte, macSize)
	if _, err := rand.Read(master); err != nil {
		return nil, err
	}
	if err := rsa.DecryptPKCS1v15SessionKey(nil, key, d[10:], master); err != nil {
		return nil, errors.New("ssl2: invalid master key ciphertext length")
	}
	return master, nil
}

// recordMAC is the SSL 2.0 keyed MD5 over the secret, the content and the
// 32-bit sequence number.
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

func appendU16(b []byte, v int) []byte {
	return append(b, byte(v>>8), byte(v))
}
