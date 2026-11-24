// SPDX-License-Identifier: GPL-3.0-only
// Go adaptation of EA Nation Server's CryptSSC2 and LobbyTagField, 2026-11-24.
package aries

import (
	"encoding/hex"
	"errors"
	"hash/crc32"
	"net/url"
	"strings"
)

// SSC2Key is the wire obfuscation key advertised by the legacy Aries backend.
const SSC2Key = "51ba8aee64ddfacae5baefa6bf61e009"

// DecodePassword ports the upstream CryptSSC2 and LobbyTagField password path.
// SSC2 is a compatibility encoding. Account storage needs its own password hash.
func DecodePassword(field string) ([]byte, error) {
	if len(field) >= 2 && field[0] == '"' && field[len(field)-1] == '"' {
		field = field[1 : len(field)-1]
	}
	encoded, err := url.PathUnescape(strings.TrimPrefix(field, "~"))
	if err != nil || len(encoded) == 0 || len(encoded) > 31 {
		return nil, errors.New("invalid Aries password field")
	}
	key, _ := hex.DecodeString(SSC2Key)
	var box [256]byte
	for i := range box {
		box[i] = byte(i)
	}
	var crc uint32
	step := func(b byte) {
		crc = crc>>8 ^ crc32.IEEETable[byte(crc)^b]
	}
	for i := range len(key) * len(box) {
		step(key[i%len(key)])
		step(box[byte(i)])
		j := byte(crc)
		box[byte(i)], box[j] = box[j], box[byte(i)]
	}

	crc = 0
	var stream byte
	password := make([]byte, 0, len(encoded))
	for i := range len(encoded) {
		b := encoded[i]
		if b < 32 || b > 127 {
			return nil, errors.New("invalid SSC2 character")
		}
		step(box[byte(i)])
		k, j := byte(i+1), byte(crc)
		a, z := box[k], box[j]
		box[k], box[j] = z, a
		stream ^= box[a-z]
		v := byte(32 + (64+int(b)-int(stream)%96)%96)
		if v == 127 {
			break
		}
		password = append(password, v)
	}
	if len(password) == 0 {
		return nil, errors.New("empty Aries password")
	}
	return password, nil
}
