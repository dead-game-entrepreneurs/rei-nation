// Package relay is the UDP relay for GoldenEye players whose NATs block direct play,
// ported from the TimeSplitters relay. See docs/games/gera/internet-play.md#relay.
package relay

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
)

// message is one JSON control line: add, remove, probe, close and ping from the
// server; hello, added, probed, closed, failed and pong from the node.
type message struct {
	Op       string            `json:"op"`
	ID       string            `json:"id,omitempty"`
	Token    string            `json:"token,omitempty"`
	PublicIP string            `json:"public_ip,omitempty"`
	Name     string            `json:"name,omitempty"`
	Room     string            `json:"room,omitempty"`
	Members  map[string]string `json:"members,omitempty"` // key -> ip:port
	Keys     []string          `json:"keys,omitempty"`
	Target   string            `json:"target,omitempty"` // ip:port
	Ports    map[string]int    `json:"ports,omitempty"`
	Error    string            `json:"error,omitempty"`
	// Reachable and RTT answer a probe.
	Reachable bool    `json:"reachable,omitempty"`
	RTT       float64 `json:"rtt_ms,omitempty"`
	// Traffic totals reported when a room closes.
	Forwarded int64   `json:"forwarded,omitempty"`
	Dropped   int64   `json:"dropped,omitempty"`
	Rejected  int64   `json:"rejected,omitempty"`
	IdleS     float64 `json:"idle_s,omitempty"`
}

const maxLine = 4096

// probeRequest is a PROB request code; consoles answer any PROB with code 0
// (FUN_004b7968 in GoldenEye). See internet-play.md#relay.
const probeRequest = 0xC0000000

// probePacket is a PROB request as TimeSplitters' relay sends it: a 0xFFFF
// prefix, then an Aries frame.
func probePacket(tid uint32) []byte {
	body := "TID=" + strconv.FormatUint(uint64(tid), 10) + "\nTYPE=1\n\x00"
	var b bytes.Buffer
	b.Write([]byte{0xff, 0xff})
	b.WriteString("PROB")
	binary.Write(&b, binary.BigEndian, uint32(probeRequest))
	binary.Write(&b, binary.BigEndian, uint32(12+len(body)))
	b.WriteString(body)
	return b.Bytes()
}

// probeReplyTID returns the TID of a PROB answer (code 0).
func probeReplyTID(data []byte) (uint32, bool) {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xff {
		data = data[2:]
	}
	if len(data) < 12 || len(data) > 128 || string(data[:4]) != "PROB" {
		return 0, false
	}
	if binary.BigEndian.Uint32(data[4:8]) != 0 || int(binary.BigEndian.Uint32(data[8:12])) != len(data) {
		return 0, false
	}
	for line := range strings.SplitSeq(strings.TrimRight(string(data[12:]), "\x00"), "\n") {
		if v, ok := strings.CutPrefix(line, "TID="); ok && len(v) <= 10 {
			if n, err := strconv.ParseUint(v, 10, 32); err == nil {
				return uint32(n), true
			}
		}
	}
	return 0, false
}
