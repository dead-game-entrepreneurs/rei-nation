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
	if err := room.add(2, guest.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatal(err)
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
