package fesl

import (
	"bytes"
	"testing"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
)

func TestDecodeRequest(t *testing.T) {
	// Synthetic fsys Hello: service, kind 0xc0 with sequence 1, size, body, NUL.
	body := "TXN=Hello\nclientString=gera\nclientPlatform=PS2\nfragmentSize=2048\n"
	wire := append([]byte("fsys\xc0\x00\x00\x01\x00\x00\x00\x00"), body...)
	wire = append(wire, 0)
	wire[11] = byte(len(wire))
	f, err := aries.Read(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	m := Decode(f)
	if m.Service != "fsys" || m.TXN != "Hello" || m.Kind() != 0xc0 || m.Sequence() != 1 {
		t.Fatalf("decoded %+v", m)
	}
	if v, ok := m.Get("clientString"); !ok || v != "gera" {
		t.Fatalf("clientString = %q, %v", v, ok)
	}
	if _, ok := m.Get("clientstring"); ok {
		t.Fatal("field keys must keep their case")
	}
}

func TestReplyEchoesCodeAndOrdersFields(t *testing.T) {
	req := Message{Service: "fsys", Code: 0xc0000007}
	var buf bytes.Buffer
	if err := Reply(req, "Hello", Field{"theaterIp", "192.0.2.10"}, Field{"theaterPort", "18041"}).Write(&buf); err != nil {
		t.Fatal(err)
	}
	want := "fsys\xc0\x00\x00\x07\x00\x00\x00\x3eTXN=Hello\ntheaterIp=192.0.2.10\ntheaterPort=18041\n\x00"
	if buf.String() != want {
		t.Fatalf("wire = %q, want %q", buf.String(), want)
	}
}
