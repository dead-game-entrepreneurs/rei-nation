package ssl2

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestPS2HandshakeAndAriesStream(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	if err := right.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	want := bytes.Repeat([]byte("Aries"), 14000)
	cert := []byte("synthetic RSA certificate bytes")
	go func() {
		conn, err := (&Server{cert: cert, key: key}).Accept(left)
		if err == nil {
			buf := make([]byte, len(want))
			if _, err = io.ReadFull(conn, buf); err == nil {
				_, err = conn.Write(buf)
			}
		}
		serverDone <- err
	}()
	client := &stream{Conn: right}
	challenge := []byte("abcdef0123456789")
	if err := client.writeRecord(append([]byte{1, 0, 2, 0, 3, 0, 0, 0, 16, 1, 0, 0x80}, challenge...)); err != nil {
		t.Fatal(err)
	}
	hello, err := client.readRecord()
	if err != nil || len(hello) != 30+len(cert) || !bytes.Equal(hello[11:11+len(cert)], cert) {
		t.Fatalf("ServerHello: %x, %v", hello, err)
	}
	connID := hello[len(hello)-16:]
	master := []byte("0123456789abcdef")
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, &key.PublicKey, master)
	if err != nil {
		t.Fatal(err)
	}
	masterKey := binary.BigEndian.AppendUint16([]byte{2, 1, 0, 0x80, 0, 0}, uint16(len(encrypted)))
	masterKey = append(masterKey, 0, 0)
	if err := client.writeRecord(append(masterKey, encrypted...)); err != nil {
		t.Fatal(err)
	}
	client.in.enable(master, '0', challenge, connID)
	client.out.enable(master, '1', challenge, connID)
	if verify, err := client.readRecord(); err != nil || !bytes.Equal(verify, append([]byte{5}, challenge...)) {
		t.Fatalf("ServerVerify: %x, %v", verify, err)
	}
	if err := client.writeRecord(append([]byte{3}, connID...)); err != nil {
		t.Fatal(err)
	}
	if finished, err := client.readRecord(); err != nil || len(finished) != 17 || finished[0] != 6 {
		t.Fatalf("ServerFinished: %x, %v", finished, err)
	}
	if _, err := client.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(client, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Aries stream did not round-trip: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
