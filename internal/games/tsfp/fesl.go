package tsfp

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

const fragmentLimit = 2016
const fragmentFlags = 0x30000000

type fesl struct {
	opts      game.Options
	keys      *loginKeys
	theater   *theater
	messenger *messenger
	content   storage.TSFPStore
	ranking   storage.TSFPRankStore
	mu        sync.Mutex
	clients   map[*aries.Client]*feslSession
}

type feslSession struct {
	hello          bool
	account        int64
	persona        storage.Persona
	rankID         string
	rankConnection string
	maps           []storage.TSFPMap
	fragments      map[fragmentID]*fragment
}

type fragmentID struct {
	tag  string
	code uint32
}

type fragment struct {
	size   int
	buffer strings.Builder
}

func (s *feslSession) receive(in aries.Frame) (aries.Frame, bool, error) {
	code := binary.BigEndian.Uint32(in.Code[:])
	if code&0x20000000 == 0 {
		return in, true, nil
	}
	f := fields(in)
	size, ok := decimal(f["SIZE"], base64.StdEncoding.EncodedLen(1<<20))
	data := f["DATA"]
	errInvalid := errors.New("invalid FESL fragment")
	if !ok || size == 0 || size%4 != 0 || len(data) == 0 || len(data) > fragmentLimit || len(data)%4 != 0 {
		return aries.Frame{}, false, errInvalid
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return aries.Frame{}, false, errInvalid
	}
	if s.fragments == nil {
		s.fragments = make(map[fragmentID]*fragment)
	}
	key := fragmentID{in.Tag, code & 0xffffff}
	part := s.fragments[key]
	if part == nil {
		if len(s.fragments) >= 8 {
			return aries.Frame{}, false, errInvalid
		}
		part = &fragment{size: size}
		s.fragments[key] = part
	}
	if part.size != size || part.buffer.Len()+len(data) > size {
		return aries.Frame{}, false, errInvalid
	}
	part.buffer.WriteString(data)
	if part.buffer.Len() < size {
		return aries.Frame{}, false, nil
	}
	delete(s.fragments, key)
	body, err := base64.StdEncoding.DecodeString(part.buffer.String())
	if err != nil || len(body) > 1<<20 {
		return aries.Frame{}, false, errInvalid
	}
	in.Body = strings.TrimSuffix(string(body), "\x00")
	binary.BigEndian.PutUint32(in.Code[:], code&^fragmentFlags)
	return in, true, nil
}

func feslFrames(in aries.Frame, pairs ...string) []aries.Frame {
	out := packet(in.Tag, pairs...)
	out.Code = in.Code
	if len(out.Body)+1 <= 2048 {
		return []aries.Frame{out}
	}
	body := base64.StdEncoding.EncodeToString([]byte(out.Body + "\x00"))
	size := strconv.Itoa(len(out.Body) + 1)
	code := binary.BigEndian.Uint32(out.Code[:]) | fragmentFlags
	var frames []aries.Frame
	for len(body) > 0 {
		n := min(len(body), fragmentLimit)
		part := packet(out.Tag, "decodedSize", size, "data", body[:n])
		binary.BigEndian.PutUint32(part.Code[:], code)
		frames = append(frames, part)
		body = body[n:]
	}
	return frames
}

func (f *fesl) Handle(ctx context.Context, c *aries.Client, in aries.Frame) {
	f.mu.Lock()
	s := f.clients[c]
	if s == nil {
		s = &feslSession{}
		f.clients[c] = s
	}
	f.mu.Unlock()
	in, ready, err := s.receive(in)
	if err != nil {
		c.CloseWithReason("invalid FESL fragment")
		return
	}
	if ready {
		for _, out := range f.respond(ctx, s, in) {
			c.Send(out)
		}
	}
}

func (f *fesl) Disconnected(c *aries.Client) {
	f.mu.Lock()
	delete(f.clients, c)
	f.mu.Unlock()
}

func (f *fesl) respond(ctx context.Context, s *feslSession, in aries.Frame) []aries.Frame {
	values := fields(in)
	txn := values["TXN"]
	if len(txn) == 0 || len(txn) > 40 {
		return nil
	}
	command := strings.ToLower(txn)
	var pairs []string
	switch in.Tag {
	case "fsys":
		switch command {
		case "hello":
			ip := f.opts.PublicIP.String()
			replies := feslFrames(in, "TXN", txn, "theaterIp", ip, "theaterPort", strconv.Itoa(theaterPort),
				"messengerIp", ip, "messengerPort", strconv.Itoa(messengerPort), "addressRemapping", "",
				"activityTimeoutSecs", "200")
			if !s.hello {
				s.hello = true
				replies = append(replies, packet("fsys", "TXN", "MemCheck", "memcheck.[]", "0", "salt", "0", "type", "0"))
			}
			return replies
		case "ping":
			if binary.BigEndian.Uint32(in.Code[:])&0xffffff == 0 {
				return nil
			}
			pairs = []string{}
		}
	case "acct":
		pairs = f.account(ctx, s, command, values)
	case "rank":
		pairs = f.rank(ctx, s, in, command, values)
	case "blob", "chnk", "fdbk":
		pairs = f.mapTransaction(ctx, s, in.Tag, command, values)
	}
	if pairs == nil {
		return nil
	}
	replies := feslFrames(in, append([]string{"TXN", txn}, pairs...)...)
	if len(replies) > 240 {
		return feslFrames(in, "TXN", txn, "errorCode", "21")
	}
	return replies
}

func failure(code string) []string { return []string{"errorCode", code} }

func validIdentity(name string) bool {
	return len(name) > 0 && len(name) <= 32 && !strings.ContainsFunc(name, func(r rune) bool {
		return r <= ' ' || r >= 127 || r == '@' || r == '/'
	})
}

func (f *fesl) account(ctx context.Context, s *feslSession, command string, values map[string]string) []string {
	store := f.opts.Store
	name, password := values["NAME"], values["PASSWORD"]
	switch command {
	case "gettos":
		return []string{"tos", "Rei Nation community server"}
	case "getcountrylist":
		return []string{"countryList.[]", "1", "countryList.0.description", "USA", "countryList.0.ISOCode", "US",
			"countryList.0.parentalControlAgeLimit", "13", "countryList.0.registrationAgeLimit", "18"}
	case "suggestscreennames", "suggestsubscreennames":
		return f.suggest(ctx, command, values)
	case "addaccount":
		if !validIdentity(name) || len(password) == 0 || len(password) > 72 ||
			strings.ContainsRune(values["EMAIL"], 0) || len(values["EMAIL"]) > 254 {
			return failure("21")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return failure("99")
		}
		err = store.CreateAccount(ctx, storage.Account{Name: name, Hash: string(hash), Mail: values["EMAIL"], Loc: values["COUNTRYCODE"]})
		if err != nil {
			return accountError(err)
		}
		return []string{}
	case "login":
		s.account, s.persona, s.maps = 0, storage.Persona{}, nil
		s.rankConnection = token(16)
		a, found, err := store.Account(ctx, name)
		if err != nil {
			return failure("99")
		}
		if !found {
			return failure("101")
		}
		if !verifyPassword(a.Hash, password) {
			return failure("122")
		}
		s.account = a.ID
		return []string{"lkey", f.keys.issue(storage.Persona{}), "userId", strconv.FormatInt(a.ID, 10)}
	case "logout":
		s.account, s.persona, s.maps = 0, storage.Persona{}, nil
		s.rankConnection = ""
		return []string{}
	}
	if s.account == 0 {
		return failure("101")
	}
	switch command {
	case "addsubaccount":
		if !validIdentity(name) {
			return failure("21")
		}
		if err := store.CreatePersona(ctx, s.account, name); err != nil {
			return accountError(err)
		}
		return []string{}
	case "getsubaccounts", "loginsubaccount":
		personas, err := store.Personas(ctx, s.account)
		if err != nil {
			return failure("99")
		}
		if command == "loginsubaccount" {
			s.persona, s.maps = storage.Persona{}, nil
			s.rankConnection = token(16)
			for _, p := range personas {
				if strings.EqualFold(p.Name, name) {
					s.persona = p
					return []string{"lkey", f.keys.issue(p), "userId", strconv.FormatInt(p.ID, 10)}
				}
			}
			return failure("101")
		}
		sort.Slice(personas, func(i, j int) bool { return personas[i].ID < personas[j].ID })
		pairs := []string{"subAccounts.[]", strconv.Itoa(len(personas))}
		for i, p := range personas {
			pairs = append(pairs, "subAccounts."+strconv.Itoa(i), p.Name)
		}
		return pairs
	}
	return nil
}

func accountError(err error) []string {
	if errors.Is(err, storage.ErrConflict) {
		return failure("160")
	}
	return failure("99")
}

func (f *fesl) suggest(ctx context.Context, command string, values map[string]string) []string {
	limit, err := strconv.Atoi(fallback(values, "MAXSUGGESTIONS", "5"))
	if err != nil {
		limit = 5
	}
	limit = min(max(0, limit), 5)
	base := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return -1
	}, values["NAME"])
	if base == "" {
		base = "Player"
	}
	pairs := []string{"names.[]", "0"}
	for n := 1; n <= 128 && (len(pairs)-2)/2 < limit; n++ {
		suffix := strconv.Itoa(n)
		name := base[:min(len(base), 11-len(suffix))] + suffix
		var found bool
		var err error
		if command == "suggestsubscreennames" {
			if f.messenger.store == nil {
				break
			}
			_, found, err = f.messenger.store.TSFPPersona(ctx, name)
		} else {
			_, found, err = f.opts.Store.Account(ctx, name)
		}
		if err != nil {
			return failure("99")
		}
		if !found {
			pairs = append(pairs, "names."+strconv.Itoa((len(pairs)-2)/2), name)
		}
	}
	pairs[1] = strconv.Itoa((len(pairs) - 2) / 2)
	return pairs
}
