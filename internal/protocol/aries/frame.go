// Package aries implements EA Nation's Aries TCP message framing.
package aries

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	headerSize = 12
	MaxFrame   = 64 * 1024
)

type Frame struct {
	Tag  string
	Code [4]byte
	Body string
}

func Read(r io.Reader) (Frame, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	size := binary.BigEndian.Uint32(header[8:])
	if size < headerSize || size > MaxFrame {
		return Frame{}, fmt.Errorf("invalid Aries frame size %d", size)
	}
	tag := string(header[:4])
	if !validTag(tag) {
		return Frame{}, errors.New("invalid Aries command tag")
	}
	body := make([]byte, int(size)-headerSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return Frame{}, err
	}
	return Frame{Tag: tag, Code: [4]byte(header[4:8]), Body: strings.TrimSuffix(string(body), "\x00")}, nil
}

// Reply accepts the four-character command or an eight-character command/status.
func Reply(tag string, fields map[string]string) Frame {
	return reply(tag, fields, "\n")
}

func SpaceReply(tag string, fields map[string]string) Frame {
	return reply(tag, fields, " ")
}

func TabReply(tag string, fields map[string]string) Frame {
	return reply(tag, fields, "\t")
}

func reply(tag string, fields map[string]string, separator string) Frame {
	f := Frame{Tag: tag}
	if len(tag) == 8 {
		f.Tag, f.Code = tag[:4], [4]byte([]byte(tag[4:]))
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if b.Len() > 0 {
			b.WriteString(separator)
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(fields[k])
	}
	f.Body = b.String()
	return f
}

func (f Frame) Write(w io.Writer) error {
	if !validTag(f.Tag) {
		return errors.New("invalid Aries command tag")
	}
	size := headerSize
	if f.Body != "" {
		size += len(f.Body) + 1
	}
	if size > MaxFrame {
		return errors.New("Aries reply exceeds frame limit")
	}
	buf := make([]byte, size)
	copy(buf[:4], f.Tag)
	copy(buf[4:8], f.Code[:])
	binary.BigEndian.PutUint32(buf[8:12], uint32(size))
	copy(buf[12:], f.Body)
	n, err := w.Write(buf)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	return err
}

func validTag(tag string) bool {
	if len(tag) != 4 {
		return false
	}
	for i := range len(tag) {
		if tag[i] < '!' || tag[i] > '~' {
			return false
		}
	}
	return true
}

// Fields decodes the newline-delimited dialect. Values remain byte strings.
func (f Frame) Fields() map[string]string {
	fields := make(map[string]string)
	for line := range strings.SplitSeq(f.Body, "\n") {
		key, val, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if ok {
			key = strings.ToUpper(strings.TrimSpace(key))
			if _, exists := fields[key]; !exists {
				fields[key] = val
			}
		}
	}
	return fields
}
