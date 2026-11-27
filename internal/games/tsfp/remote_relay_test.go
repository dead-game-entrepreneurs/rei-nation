package tsfp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/tsfp/relaynode"
)

func TestRemoteRelayControlAndForwarding(t *testing.T) {
	certFile, keyFile := relayCertificate(t)
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	secret := "synthetic-relay-token"
	if err := os.WriteFile(tokenFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relayIP := netip.MustParseAddr("127.0.0.22")
	tokensFile := filepath.Join(directory, "tokens.txt")
	digest := sha256.Sum256([]byte(secret))
	credentials := fmt.Sprintf("test sha256:%x %s\n", digest, relayIP)
	if err := os.WriteFile(tokensFile, []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
	hub, err := newRemoteRelayHub(relayIP, tokensFile, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	hub.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 2)
	go func() { done <- hub.serve(ctx, listener) }()
	go func() {
		done <- relaynode.Run(ctx, relaynode.Config{Server: listener.Addr().String(), CAFile: certFile,
			TokenFile: tokenFile, Name: "test", PublicIP: relayIP, Bind: relayIP,
			FirstPort: 22000, LastPort: 22099, AllowPrivate: true,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	t.Cleanup(func() {
		cancel()
		for range 2 {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	})
	for deadline := time.Now().Add(3 * time.Second); !hub.available(); {
		if time.Now().After(deadline) {
			t.Fatal("relay did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	host, guest := testUDP(t, "127.0.0.2"), testUDP(t, "127.0.0.3")
	room := hub.room("synthetic")
	if err := room.add(1, host.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatal(err)
	}
	remote := room.(*remoteRelayRoom)
	if _, err := hub.request(relayControlMessage{Op: "close", Room: remote.id}, time.Second); err != nil {
		t.Fatalf("simulate relay room loss: %v", err)
	}
	if err := room.add(2, guest.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatalf("restore room after relay loss: %v", err)
	}
	if !room.address(1).IsValid() || !room.address(2).IsValid() {
		t.Fatal("restored room is missing a member port")
	}
	if _, err := host.WriteToUDPAddrPort([]byte("host mapping"), room.address(2)); err != nil {
		t.Fatal(err)
	}
	if data, source := receiveUDP(t, guest); string(data) != "host mapping" || source != room.address(1) {
		t.Fatalf("guest received %q from %s", data, source)
	}
	if _, err := guest.WriteToUDPAddrPort([]byte("guest reply"), room.address(1)); err != nil {
		t.Fatal(err)
	}
	if data, source := receiveUDP(t, host); string(data) != "guest reply" || source != room.address(2) {
		t.Fatalf("host received %q from %s", data, source)
	}
	if _, err := host.WriteToUDPAddrPort([]byte("host reply"), room.address(2)); err != nil {
		t.Fatal(err)
	}
	if data, source := receiveUDP(t, guest); string(data) != "host reply" || source != room.address(1) {
		t.Fatalf("guest received %q from %s", data, source)
	}
	room.close()
}

func TestRemoteRelayServeClosesUnauthenticatedConnections(t *testing.T) {
	certFile, keyFile := relayCertificate(t)
	directory := t.TempDir()
	tokensFile := filepath.Join(directory, "tokens.txt")
	relayIP := netip.MustParseAddr("127.0.0.22")
	digest := sha256.Sum256([]byte("synthetic-relay-token"))
	if err := os.WriteFile(tokensFile, []byte(fmt.Sprintf("test sha256:%x %s\n", digest, relayIP)), 0o600); err != nil {
		t.Fatal(err)
	}
	hub, err := newRemoteRelayHub(relayIP, tokensFile, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	hub.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- hub.serve(ctx, listener) }()
	conn, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0x16}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay hub did not wait for and close an unauthenticated handler")
	}
}

func relayCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay-control"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certFile, keyFile := filepath.Join(directory, "cert.pem"), filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
