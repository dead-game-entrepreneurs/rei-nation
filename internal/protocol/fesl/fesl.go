// Package fesl implements EA FESL message bodies and header codes.
//
// FESL shares the Aries 12-byte header: a four-character service, a
// big-endian code word, and a big-endian size that includes the header. The
// body is newline-separated key=value text ending in NUL. This package reads and
// writes frames through package aries and adds the FESL meaning of the code
// word and the transaction body.
package fesl

import (
	"encoding/binary"
	"strings"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
)

// Field is one key=value pair. Replies keep fields in the order given because
// FESL clients read transaction text in order and TXN conventionally leads.
type Field struct {
	Key   string
	Value string
}

// Message is a decoded FESL transaction.
type Message struct {
	Service string
	// Code is the raw header word. Its top byte is the message kind and the low
	// 24 bits are the client's transaction sequence number.
	Code   uint32
	TXN    string
	fields map[string]string
}

// Decode parses a frame body. Keys keep their case; values are unquoted and
// percent-decoded. TXN is matched without regard to case because clients and
// servers differ in capitalization.
func Decode(f aries.Frame) Message {
	m := Message{Service: f.Tag, Code: binary.BigEndian.Uint32(f.Code[:]), fields: make(map[string]string)}
	for line := range strings.SplitSeq(f.Body, "\n") {
		key, val, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if !ok {
			continue
		}
		val = decodeValue(val)
		if _, exists := m.fields[key]; !exists {
			m.fields[key] = val
		}
		if m.TXN == "" && strings.EqualFold(key, "TXN") {
			m.TXN = val
		}
	}
	return m
}

// Get returns a field by its exact key.
func (m Message) Get(key string) (string, bool) {
	v, ok := m.fields[key]
	return v, ok
}

// Fields returns a copy of every decoded field.
func (m Message) Fields() map[string]string {
	out := make(map[string]string, len(m.fields))
	for k, v := range m.fields {
		out[k] = v
	}
	return out
}

// Prefixed returns the fields whose key starts with prefix (case-insensitive),
// with each returned key upper-cased. The theater carries game, detail and
// player attributes as b-, d- and p- prefixed fields.
func (m Message) Prefixed(prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range m.fields {
		if len(k) >= len(prefix) && strings.EqualFold(k[:len(prefix)], prefix) {
			out[strings.ToUpper(k)] = v
		}
	}
	return out
}

// GetFold returns a field by a case-insensitive key match. Clients vary the
// case of field names between transactions.
func (m Message) GetFold(key string) (string, bool) {
	if v, ok := m.fields[key]; ok {
		return v, ok
	}
	for k, v := range m.fields {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// Kind returns the top byte of the header code word.
func (m Message) Kind() byte { return byte(m.Code >> 24) }

// Sequence returns the client's transaction sequence number.
func (m Message) Sequence() uint32 { return m.Code & 0x00ffffff }

// Reply answers a request with the same code word, so the client can match
// the reply to its transaction.
func Reply(req Message, txn string, fields ...Field) aries.Frame {
	return Encode(req.Service, req.Code, txn, fields...)
}

// Packet builds a frame whose command is the service tag and whose code word is
// a status, with a body of ordered key=value fields and no TXN line. The
// messenger and theater services use this shape; the acct and fsys services use
// Encode, which leads with TXN.
func Packet(service string, code uint32, fields ...Field) aries.Frame {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(f.Key)
		b.WriteByte('=')
		b.WriteString(encodeValue(f.Value))
		b.WriteByte('\n')
	}
	fr := aries.Frame{Tag: service, Body: b.String()}
	binary.BigEndian.PutUint32(fr.Code[:], code)
	return fr
}

// Encode builds a frame with TXN first and the remaining fields in order.
// Values are percent-encoded so control characters, delimiters and non-ASCII
// bytes survive the newline-delimited body; a value containing a space is
// quoted.
func Encode(service string, code uint32, txn string, fields ...Field) aries.Frame {
	var b strings.Builder
	b.WriteString("TXN=")
	b.WriteString(encodeValue(txn))
	for _, f := range fields {
		b.WriteByte('\n')
		b.WriteString(f.Key)
		b.WriteByte('=')
		b.WriteString(encodeValue(f.Value))
	}
	b.WriteByte('\n')
	f := aries.Frame{Tag: service, Body: b.String()}
	binary.BigEndian.PutUint32(f.Code[:], code)
	return f
}

// needsEscape reports the bytes the FESL dialect percent-encodes: control and
// non-ASCII bytes, and the delimiters that would otherwise be read as syntax.
func needsEscape(c byte) bool {
	return c < 0x20 || c >= 0x7f || c == '=' || c == '"' || c == ':' || c == '%' || c == '^'
}

func encodeValue(s string) string {
	escape := false
	space := false
	for i := 0; i < len(s); i++ {
		if needsEscape(s[i]) {
			escape = true
		}
		if s[i] == ' ' {
			space = true
		}
	}
	if !escape && !space {
		return s
	}
	const hex = "0123456789abcdef"
	var b strings.Builder
	if space {
		b.WriteByte('"')
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if needsEscape(c) {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		} else {
			b.WriteByte(c)
		}
	}
	if space {
		b.WriteByte('"')
	}
	return b.String()
}

func decodeValue(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
