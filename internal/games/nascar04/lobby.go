package nascar04

import (
	"crypto/rand"
	"encoding/binary"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
)

type room struct {
	id         int
	name, desc string
	password   string
	limit      int
	permanent  bool
	members    map[*player]bool
}

const defaultRoomName = "LVL.1"

func initialRooms() map[int]*room {
	rooms := make(map[int]*room)
	for i, name := range []string{defaultRoomName, "E.Open Race", "W.Open Race", "B.Rookie Race"} {
		id := i + 1
		rooms[id] = &room{id: id, name: name, limit: 50, permanent: true, members: make(map[*player]bool)}
	}
	return rooms
}

type challenge struct {
	to      *player
	host    bool
	expires time.Time
}

func (l *lobby) handleLobby(p *player, tag string, f map[string]string) {
	if p.persona == "" || l.online[strings.ToLower(p.persona)] != p {
		p.client.Send(aries.Reply(tag+"maut", nil))
		return
	}
	switch tag {
	case "rcat":
		l.leave(p)
		p.client.Send(aries.Reply("rcat", map[string]string{"CAT0": "Default,Global,LVL,,,,,1"}))
		l.listRooms(p)
	case "arom":
		count := 0
		for f["CAT"+strconv.Itoa(count)] != "" {
			count++
		}
		p.client.Send(aries.Reply("arom", map[string]string{"COUNT": strconv.Itoa(count)}))
		l.listRooms(p)
	case "room":
		l.createRoom(p, f)
	case "move":
		l.move(p, f)
	case "peek":
		l.peek(p, l.findRoom(f))
	case "auxi":
		if value, ok := f["TEXT"]; ok && wireText(value, 2048) {
			p.proposal = value
		}
		p.client.Send(aries.Reply("auxi", map[string]string{"TEXT": p.proposal}))
		if p.room != nil {
			for viewer := range p.room.members {
				sendRoomMember(viewer, p)
			}
		}
	case "mesg":
		l.message(p, f)
	case "chal":
		l.challenge(p, f)
	case "llvl":
		p.client.Send(aries.Reply("llvl", map[string]string{"SKILL_PTS": "0", "SKILL_LVL": "0", "SKILL": ""}))
		sendPlayerState(p)
		l.status(p)
	case "qdef":
		p.client.Send(aries.Reply("qdef", quickMessages()))
	case "usld":
		fields := quickMessages()
		fields["UID"] = strconv.FormatInt(p.account.ID, 10)
		p.client.Send(aries.Reply("usld", fields))
	case "priv":
		priv := "1"
		if f["MODE"] == "off" {
			priv = "0"
		}
		p.client.Send(aries.Reply("priv", map[string]string{"PRIV": priv}))
	default:
		l.opts.Log.Debug("unsupported Aries command", "command", tag)
		p.client.Send(aries.Reply(tag+"unsp", nil))
	}
}

func quickMessages() map[string]string {
	return map[string]string{
		"IMGATE": "0", "SPM_EA": "0", "SPM_PART": "0",
		"QMSG0": "\"Do you want to play a game?\"", "QMSG1": "Yes", "QMSG2": "No",
		"QMSG3": "\"OK, let's start\"", "QMSG4": "\"Have fun!\"", "QMSG5": "\"Good game!\"",
		"QMSG6": "Thanks!", "QMSG7": "\"Catch you later\"", "QMSG8": "\"See Ya!\"",
	}
}

func (l *lobby) createRoom(p *player, f map[string]string) {
	name, desc, password := nameValue(f["NAME"]), f["DESC"], nameValue(f["PASS"])
	if name == "" || !wireText(name, 32) || !wireText(desc, 132) || !wireText(password, 32) ||
		l.findRoom(map[string]string{"NAME": name}) != nil {
		p.client.Send(aries.Reply("roomfull", nil))
		return
	}
	limit, err := strconv.Atoi(f["MAX"])
	if err != nil || limit < 1 || limit > 50 {
		limit = 50
	}
	r := &room{id: l.nextRoom, name: name, desc: desc, password: password, limit: limit, members: make(map[*player]bool)}
	l.nextRoom++
	l.rooms[r.id] = r
	l.join(p, r, "room", name)
}

func (l *lobby) findRoom(f map[string]string) *room {
	if id := f["IDENT"]; id != "" {
		n, err := strconv.Atoi(id)
		if err == nil {
			return l.rooms[n]
		}
		return nil
	}
	name := nameValue(f["NAME"])
	for _, r := range l.rooms {
		if strings.EqualFold(r.name, name) {
			return r
		}
	}
	return nil
}

func (l *lobby) peek(p *player, r *room) {
	if (p.room == nil || p.room.name == defaultRoomName) && p.preview != r {
		// The preview reads the client room name from +who and compares
		// it with the selected room before displaying the roster.
		p.preview = r
		sendPlayerState(p)
	}
	if r == nil {
		p.client.Send(aries.Reply("peek", nil))
		return
	}
	for member := range r.members {
		sendRoomMember(p, member)
	}
	count := strconv.Itoa(len(r.members))
	p.client.Send(aries.Reply("peek", map[string]string{"NAME": r.name, "IDENT": strconv.Itoa(r.id), "COUNT": count, "T": count}))
}

func (l *lobby) move(p *player, f map[string]string) {
	if f["IDENT"] == "0" || f["IDENT"] == "" && nameValue(f["NAME"]) == "" {
		l.leave(p)
		p.client.Send(aries.Reply("move", nil))
		return
	}
	r := l.findRoom(f)
	switch {
	case r == nil:
		p.client.Send(aries.Reply("moveurom", nil))
	case r.password != nameValue(f["PASS"]) && p.room != r:
		p.client.Send(aries.Reply("movepass", nil))
	case len(r.members) >= r.limit && p.room != r:
		p.client.Send(aries.Reply("movefull", nil))
	default:
		l.join(p, r, "move", r.name)
	}
}

func (l *lobby) join(p *player, r *room, tag, name string) {
	if p.room != r {
		l.leave(p)
		p.room = r
		r.members[p] = true
	}
	p.preview = nil
	p.client.Send(aries.Reply(tag, map[string]string{
		"IDENT": strconv.Itoa(r.id), "NAME": name, "COUNT": strconv.Itoa(len(r.members)),
		"FLAGS": "A", "LIMIT": strconv.Itoa(r.limit), "DESC": r.desc,
	}))
	l.announceRoom(r)
	for viewer := range r.members {
		// The client clears its user list when +who changes its room.
		sendPlayerState(viewer)
		for member := range r.members {
			sendRoomMember(viewer, member)
		}
	}
}

func (p *player) displayedRoom() *room {
	if p.preview != nil {
		return p.preview
	}
	return p.room
}

func sendPlayerState(p *player) {
	fields := playerInfo(p, p.displayedRoom())
	// Retail reads the current room name from R on +who. On +usr, R is numeric.
	fields["R"] = fields["RM"]
	p.client.Send(aries.Reply("+who", fields))
}

func sendRoomMember(viewer, member *player) {
	// Retail tracks the local player through +who and treats +usr rows as challenge targets.
	if viewer != member && viewer.displayedRoom() == member.room {
		fields := playerInfo(member, member.room)
		fields["T"] = fields["RT"]
		viewer.client.Send(aries.Reply("+usr", fields))
	}
}

func (l *lobby) leave(p *player) {
	l.cancelChallenge(p)
	p.proposal = ""
	p.preview = nil
	r := p.room
	if r == nil {
		return
	}
	p.room = nil
	delete(r.members, p)
	for member := range r.members {
		if member.pending.to == p {
			member.pending = challenge{}
			l.notifyChallengeDeclined(member, p)
		}
	}
	departure := aries.Reply("+usr", map[string]string{"I": strconv.FormatInt(p.personaID, 10)})
	for _, viewer := range l.online {
		if viewer.displayedRoom() == r {
			viewer.client.Send(departure)
		}
	}
	if !r.permanent && len(r.members) == 0 {
		delete(l.rooms, r.id)
		l.broadcastRoom(aries.Reply("+rom", map[string]string{"I": strconv.Itoa(r.id)}))
		return
	}
	l.announceRoom(r)
}

func (l *lobby) subscribeRooms(p *player, body string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p.client.Send(aries.SpaceReply("sele", map[string]string{"MORE": "0", "SLOTS": "4", "STATS": "0"}))
	if p.persona == "" || l.online[strings.ToLower(p.persona)] != p {
		return
	}
	// The retail ROOMS subscription is a space-delimited sele request.
	for field := range strings.FieldsSeq(body) {
		if strings.EqualFold(field, "ROOMS=1") {
			l.listRooms(p)
			return
		}
	}
}

func (l *lobby) listRooms(p *player) {
	rooms := make([]*room, 0, len(l.rooms))
	for _, r := range l.rooms {
		rooms = append(rooms, r)
	}
	// The retail browser walks contiguous rows with the same category prefix.
	slices.SortFunc(rooms, func(a, b *room) int { return strings.Compare(a.name, b.name) })
	for _, r := range rooms {
		p.client.Send(roomInfo(r))
	}
}

func (l *lobby) announceRoom(r *room) {
	l.broadcastRoom(roomInfo(r))
}

func (l *lobby) broadcastRoom(f aries.Frame) {
	for _, p := range l.online {
		p.client.Send(f)
	}
}

func roomInfo(r *room) aries.Frame {
	return aries.Reply("+rom", map[string]string{
		"I": strconv.Itoa(r.id), "N": r.name, "DN": r.name, "D": r.desc, "F": "A",
		"T": strconv.Itoa(len(r.members)), "L": strconv.Itoa(r.limit),
	})
}

func (l *lobby) status(p *player) {
	p.client.Send(aries.Reply("+sst", map[string]string{
		"UIL": strconv.Itoa(len(l.online)), "UIG": "0", "UIR": "0", "GIP": "0", "GCR": "0", "GCM": "0",
	}))
}

func playerInfo(p *player, r *room) map[string]string {
	roomID, roomName, roomCount, roomFlags := "0", defaultRoomName, "0", "CK"
	if r != nil {
		roomID, roomName, roomCount, roomFlags = strconv.Itoa(r.id), r.name, strconv.Itoa(len(r.members)), "A"
	}
	remote := p.client.Addr().String()
	local := remote
	if p.reported.IsValid() {
		local = p.reported.String()
	}
	return map[string]string{
		"I": strconv.FormatInt(p.personaID, 10), "M": p.account.Name, "N": p.persona,
		"F": "", "P": "80", "S": lobbyRecord(p.stats), "X": p.proposal, "G": "0",
		"AT": "", "CL": "511", "LV": "1049601", "MD": "0", "R": rankField(p.stats), "US": "",
		"HW": "0", "RP": strconv.Itoa(p.reputation), "LO": p.account.Loc, "CI": "0", "CT": "0",
		"A": remote, "LA": local, "C": "4000,,7,1,1,,1,1,5553",
		"RI": roomID, "RT": roomCount, "RG": "0", "RGC": "0", "RM": roomName, "RF": roomFlags,
	}
}

func (l *lobby) message(p *player, f map[string]string) {
	p.client.Send(aries.Reply("mesg", nil))
	text, attr, target := f["TEXT"], f["ATTR"], nameValue(f["PRIV"])
	if text == "" || !wireText(text, 2048) || !wireText(attr, 16) {
		return
	}
	if target != "" {
		if to := l.online[strings.ToLower(target)]; to != nil && to.room == p.room && to != p {
			if attr == "3" && p.room != nil {
				l.updateRaceOffer(p, to, nameValue(text))
			}
			sendPrivateMessage(to, p.persona, text, attr)
		}
		return
	}
	if attr == "" {
		attr = "Z"
	}
	frame := aries.TabReply("+msg", map[string]string{"T": text, "N": p.persona, "U": "", "F": attr})
	if p.room != nil {
		for member := range p.room.members {
			member.client.Send(frame)
		}
		return
	}
	// The lobby can send chat before a room is selected. Treat those
	// players as a shared channel without exposing chat from named rooms.
	for _, member := range l.online {
		if member.room == nil {
			member.client.Send(frame)
		}
	}
}

func (l *lobby) challenge(p *player, f map[string]string) {
	p.client.Send(aries.Reply("chal", nil))
	targetName := nameValue(f["PERS"])
	if targetName == "*" {
		l.cancelChallenge(p)
		return
	}
	p.pending = challenge{}
	target := l.online[strings.ToLower(targetName)]
	if target == nil || target == p || p.room == nil || target.room != p.room {
		return
	}
	now := time.Now()
	p.pending = challenge{to: target, host: f["HOST"] == "1", expires: now.Add(30 * time.Second)}
	if target.pending.to != p || now.After(target.pending.expires) {
		return
	}
	host, guest := p, target
	if !p.pending.host && target.pending.host {
		host, guest = target, p
	}
	p.clearChallengeWith(target)
	race := l.startRace(host, guest, now)
	hostIP, guestIP := host.client.Addr(), guest.client.Addr()
	var relayed bool
	if release := l.relay.admit(hostIP, guestIP); release != nil {
		race.release = append(race.release, release)
		relayed = true
	}
	if l.demangler != nil {
		race.release = append(race.release, l.demangler.Admit(strings.ToLower(host.persona), hostIP, guestIP, now.Add(20*time.Minute)))
	}
	var random [4]byte
	rand.Read(random[:])
	seed := strconv.FormatUint(uint64(binary.BigEndian.Uint32(random[:])%2147483647)+1, 10)
	when := strconv.FormatInt(race.when, 10)
	for _, peers := range [][2]*player{{host, guest}, {guest, host}} {
		self, other := peers[0], peers[1]
		address := other.client.Addr().String()
		if relayed {
			address = l.opts.PublicIP.String()
		} else if hostIP == guestIP && other.reported.IsPrivate() {
			address = other.reported.String()
		}
		self.client.Send(aries.Reply("+ses", map[string]string{
			"NAME": host.persona, "SELF": self.persona, "HOST": host.persona, "OPPO": other.persona,
			"P1": host.persona, "P2": guest.persona, "P3": "", "P4": "", "AUTH": "",
			"ADDR": address, "FROM": address, "SEED": seed, "WHEN": when,
		}))
	}
}
