package ssl2

import (
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/ssl2/ssl2test"
)

// testClient speaks the subset of SSL 2.0 the server implements, from the
// client side, so the test drives a real handshake and encrypted exchange.
type testClient struct {
	conn              net.Conn
	readSeq, writeSeq uint32
	enc, dec          *rc4.Cipher
	rk, wk            []byte
	plain             []byte
}

func (c *testClient) writeRecord(payload []byte) error {
	seq := c.writeSeq
	c.writeSeq++
	body := payload
	if c.enc != nil {
		body = make([]byte, macSize+len(payload))
		copy(body, recordMAC(c.writeKey(), payload, seq))
		copy(body[macSize:], payload)
		c.enc.XORKeyStream(body, body)
	}
	rec := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(rec[:2], 0x8000|uint16(len(body)))
	copy(rec[2:], body)
	_, err := c.conn.Write(rec)
	return err
}

func (c *testClient) readRecord() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0]&0x80 == 0 {
		return nil, errors.New("test client expects two-byte record headers")
	}
	length := int(hdr[0]&0x7f)<<8 | int(hdr[1])
	data := make([]byte, length)
	if _, err := io.ReadFull(c.conn, data); err != nil {
		return nil, err
	}
	seq := c.readSeq
	c.readSeq++
	if c.dec == nil {
		return data, nil
	}
	c.dec.XORKeyStream(data, data)
	mac, content := data[:macSize], data[macSize:]
	if !bytes.Equal(mac, recordMAC(c.readKey(), content, seq)) {
		return nil, errors.New("client saw a bad MAC")
	}
	return content, nil
}

// The client reads with the server's write key and writes with its read key.
func (c *testClient) readKey() []byte  { return c.rk }
func (c *testClient) writeKey() []byte { return c.wk }

func (c *testClient) Read(p []byte) (int, error) {
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

func handshakeClient(t *testing.T, conn net.Conn) *testClient {
	t.Helper()
	c := &testClient{conn: conn}
	challenge := make([]byte, 16)
	rand.Read(challenge)
	hello := []byte{1}
	hello = appendU16(hello, 2)
	hello = appendU16(hello, len(rc4MD5))
	hello = appendU16(hello, 0)
	hello = appendU16(hello, len(challenge))
	hello = append(hello, rc4MD5...)
	hello = append(hello, challenge...)
	if err := c.writeRecord(hello); err != nil {
		t.Fatal(err)
	}
	serverHello, err := c.readRecord()
	if err != nil {
		t.Fatal(err)
	}
	if serverHello[0] != 4 {
		t.Fatalf("expected SERVER-HELLO, got type %d", serverHello[0])
	}
	certLen := int(binary.BigEndian.Uint16(serverHello[5:7]))
	csLen := int(binary.BigEndian.Uint16(serverHello[7:9]))
	connLen := int(binary.BigEndian.Uint16(serverHello[9:11]))
	certDER := serverHello[11 : 11+certLen]
	connID := serverHello[11+certLen+csLen : 11+certLen+csLen+connLen]
	pub, err := ssl2test.PublicKeyFromCertificate(certDER)
	if err != nil {
		t.Fatalf("client could not read the server certificate key: %v", err)
	}

	master := make([]byte, 16)
	rand.Read(master)
	encMaster, err := rsa.EncryptPKCS1v15(rand.Reader, pub, master)
	if err != nil {
		t.Fatal(err)
	}
	masterKey := []byte{2}
	masterKey = append(masterKey, rc4MD5...)
	masterKey = appendU16(masterKey, 0)
	masterKey = appendU16(masterKey, len(encMaster))
	masterKey = appendU16(masterKey, 0)
	masterKey = append(masterKey, encMaster...)
	if err := c.writeRecord(masterKey); err != nil {
		t.Fatal(err)
	}
	c.wk = keyedMD5(master, []byte("1"), challenge, connID)
	c.rk = keyedMD5(master, []byte("0"), challenge, connID)
	c.enc, _ = rc4.NewCipher(c.wk)
	c.dec, _ = rc4.NewCipher(c.rk)

	verify, err := c.readRecord()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(verify, append([]byte{5}, challenge...)) {
		t.Fatal("SERVER-VERIFY did not echo the challenge")
	}
	if err := c.writeRecord(append([]byte{3}, connID...)); err != nil {
		t.Fatal(err)
	}
	finished, err := c.readRecord()
	if err != nil {
		t.Fatal(err)
	}
	if len(finished) != 17 || finished[0] != 6 {
		t.Fatalf("unexpected SERVER-FINISHED %x", finished)
	}
	return c
}

func TestHandshakeAndEncryptedEcho(t *testing.T) {
	certDER, key, err := GenerateCertificate("fesl.ea.com")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sln := NewListener(ln, certDER, key, nil)
	go func() {
		conn, err := sln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 512)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	raw, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	c := handshakeClient(t, raw)

	for _, msg := range []string{"hello world", "another encrypted line"} {
		if err := c.writeRecord([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != msg {
			t.Fatalf("echo = %q, want %q", got, msg)
		}
	}
}

func TestRejectsWrongCipher(t *testing.T) {
	certDER, key, err := GenerateCertificate("fesl.ea.com")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sln := NewListener(ln, certDER, key, nil)
	errc := make(chan error, 1)
	go func() {
		conn, err := sln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer conn.Close()
		_, err = conn.Read(make([]byte, 1))
		errc <- err
	}()

	raw, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	c := &testClient{conn: raw}
	// A ClientHello offering only a cipher the server does not implement.
	hello := []byte{1}
	hello = appendU16(hello, 2)
	hello = appendU16(hello, 3)
	hello = appendU16(hello, 0)
	hello = appendU16(hello, 16)
	hello = append(hello, 0x02, 0x00, 0x80) // SSL_CK_RC4_128_EXPORT40_WITH_MD5
	hello = append(hello, make([]byte, 16)...)
	if err := c.writeRecord(hello); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("server accepted an unsupported cipher")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not reject the handshake")
	}
}

func TestCertificateWorkaroundOIDs(t *testing.T) {
	certDER, _, err := GenerateCertificate("fesl.ea.com")
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(certDER, oidMD5WithRSA); n != 1 {
		t.Errorf("MD5-with-RSA OID count = %d, want 1 (inner signature)", n)
	}
	if n := bytes.Count(certDER, oidRSAEncryption); n != 2 {
		t.Errorf("rsaEncryption OID count = %d, want 2 (public key and outer signature)", n)
	}
	if bytes.Contains(certDER, oidSHA1WithRSA) {
		t.Error("SHA1-with-RSA OID should have been rewritten")
	}
	// Both validity dates must stay ASN.1 UTCTime (tag 0x17) for the client.
	if bytes.Contains(certDER, []byte{0x18, 0x0f}) {
		t.Error("certificate used GeneralizedTime; the client needs UTCTime")
	}
}
