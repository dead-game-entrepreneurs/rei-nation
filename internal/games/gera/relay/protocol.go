// Package relay is the UDP relay fallback for GoldenEye players whose NATs
// block a direct connection. It ports the TimeSplitters: Future Perfect relay
// (src/tsfp_online/relay.py and relay_hub.py in that project), which serves the
// same EA FESL 1.2 game-browser library.
//
// A relay node runs on its own machine and public IP: the client treats every
// datagram from the theater's IP as server traffic, so a relay sharing that IP
// would be mistaken for the server. The node dials the Rei Nation server over
// TLS, pins the server's control certificate and signs in with a token. The
// server (Hub) then asks it to open one room per match. A room holds one relay
// port per player: whatever player Y sends to X's port reaches X from Y's
// port, so every player sees each other player at one fixed relay address.
// That satisfies the client's check that peer traffic comes from the address
// it was given, and it keeps working when the host passes a relayed player's
// address on to other guests.
package relay

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
)

// message is one JSON line on the control link. Ops from the server: add,
// remove, probe, close, ping. From the node: hello, added, probed, closed,
// failed, pong.
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

// probeRequest is the code word of a PROB request. A console answers any PROB
// request with code 0 and the same TID (FUN_004b7968 -> FUN_004b7578 in the
// GoldenEye binary, the same handler TimeSplitters has at 0x592978). Sent from
// a fresh port, an answer shows the console's router lets in packets from an
// address it never sent to.
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
