package aries

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:min(1, len(p))])
}

func TestReadFragmentedAndCoalescedFrames(t *testing.T) {
	// Wire layout from the Java server: four command bytes, four status bytes,
	// big-endian total size, and an optional NUL-terminated body.
	wire := []byte("newsnew0\x00\x00\x00\x13NAME=0\x00@dir\x00\x00\x00\x00\x00\x00\x00\x0c")
	for _, fragmented := range []bool{true, false} {
		var r io.Reader = bytes.NewReader(wire)
		if fragmented {
			r = oneByteReader{r}
		}
		f, err := Read(r)
		if err != nil || f.Tag != "news" || string(f.Code[:]) != "new0" || f.Body != "NAME=0" {
			t.Fatalf("news frame, fragmented=%v: %+v %v", fragmented, f, err)
		}
		f, err = Read(r)
		if err != nil || f.Tag != "@dir" || f.Body != "" {
			t.Fatalf("directory frame, fragmented=%v: %+v %v", fragmented, f, err)
		}
	}
}

func TestFrameLimitsAndReplyLayout(t *testing.T) {
	for _, size := range []uint32{0, 11, MaxFrame + 1, 0xffffffff} {
		header := []byte("@dir\x00\x00\x00\x00\x00\x00\x00\x00")
		binary.BigEndian.PutUint32(header[8:], size)
		if _, err := Read(bytes.NewReader(header)); err == nil {
			t.Fatalf("accepted size %d", size)
		}
	}
	var buf bytes.Buffer
	if err := Reply("authimst", nil).Write(&buf); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "authimst\x00\x00\x00\x0c" {
		t.Fatalf("error reply: %q", got)
	}
}

func TestDecodePassword(t *testing.T) {
	// Public synthetic vector from the upstream Java CryptSSC2Test.
	pw, err := DecodePassword("\"~Kbx&4f|xq>a[zLz0Z%7f%25Rs9[He@ydyVb\"")
	if err != nil || string(pw) != "pass" {
		t.Fatal("SSC2 vector failed", err)
	}
	if _, err := DecodePassword("~%"); err == nil {
		t.Fatal("accepted incomplete wire escape")
	}
}
