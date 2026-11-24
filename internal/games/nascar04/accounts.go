package nascar04

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"

	"golang.org/x/crypto/bcrypt"
)

func (l *lobby) editAccount(ctx context.Context, p *player, f map[string]string) {
	name := nameValue(f["NAME"])
	current, err := aries.DecodePassword(f["PASS"])
	if l.accounts == nil || !accountName.MatchString(name) || err != nil {
		p.client.Send(aries.Reply("editpass", nil))
		return
	}
	a, found, err := l.opts.Store.Account(ctx, name)
	if err != nil {
		l.storeFailure(p.client, "edit", err)
		return
	}
	if !found || bcrypt.CompareHashAndPassword([]byte(a.Hash), current) != nil {
		p.client.Send(aries.Reply("editpass", nil))
		return
	}

	if field, present := f["MAIL"]; present {
		address := nameValue(field)
		if address != "" && !recoveryAddress(address) {
			p.client.Send(aries.Reply("editmail", nil))
			return
		}
		a.Mail = address
	}
	hash := a.Hash
	if replacement := f["CHNG"]; replacement != "" {
		password, err := aries.DecodePassword(replacement)
		if err != nil {
			p.client.Send(aries.Reply("editpass", nil))
			return
		}
		if string(password) != string(current) {
			next, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
			if err != nil {
				l.storeFailure(p.client, "edit", err)
				return
			}
			hash = string(next)
		}
	}
	changed, err := l.accounts.UpdateAccount(ctx, a, hash)
	switch {
	case errors.Is(err, storage.ErrConflict):
		p.client.Send(aries.Reply("editmail", nil))
	case err != nil:
		l.storeFailure(p.client, "edit", err)
	case !changed:
		p.client.Send(aries.Reply("editpass", nil))
	default:
		p.client.Send(aries.Reply("edit", nil))
	}
}

func (l *lobby) deletePersona(ctx context.Context, p *player, field string) {
	if p.account.ID == 0 || l.accounts == nil {
		p.client.Send(aries.Reply("dpermaut", nil))
		return
	}
	name := nameValue(field)
	if !personaName.MatchString(name) {
		p.client.Send(aries.Reply("dperiper", nil))
		return
	}
	l.identityMu.Lock()
	defer l.identityMu.Unlock()
	deleted, err := l.accounts.DeletePersona(ctx, p.account.ID, name)
	if err != nil {
		l.storeFailure(p.client, "dper", err)
		return
	}
	if deleted == "" {
		p.client.Send(aries.Reply("dperiper", nil))
		return
	}
	if p.lastMade == deleted {
		p.lastMade = ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := strings.ToLower(deleted)
	if selected := l.online[key]; selected != nil {
		l.leave(selected)
		l.closeCompanions(selected)
		delete(l.online, key)
		if selected == p {
			p.persona = ""
			p.personaID = 0
		} else {
			selected.client.Close()
		}
	} else if l.buddy != nil {
		l.buddy.revoke(deleted, nil)
	}
	p.client.Send(aries.Reply("dper", map[string]string{"PERS": deleted}))
}

func recoveryAddress(s string) bool {
	if !wireText(s, 128) || strings.ContainsAny(s, "<> ,;") {
		return false
	}
	for i := range len(s) {
		if s[i] < '!' || s[i] > '~' {
			return false
		}
	}
	address, err := mail.ParseAddress(s)
	return err == nil && address.Address == s
}

func (l *lobby) recoverAccount(ctx context.Context, p *player, f map[string]string) {
	if l.accounts == nil || l.opts.SendRecoveryMail == nil {
		p.client.Send(aries.Reply("lostneml", nil))
		return
	}
	// Both recovery forms use the same response regardless of account existence.
	defer p.client.Send(aries.Reply("lost", nil))
	address, name := nameValue(f["MAIL"]), nameValue(f["NAME"])
	var a storage.Account
	var found bool
	var err error
	switch {
	case address != "" && name == "" && recoveryAddress(address):
		a, found, err = l.accounts.AccountByMail(ctx, address)
	case address == "" && accountName.MatchString(name):
		a, found, err = l.opts.Store.Account(ctx, name)
	default:
		return
	}
	if err != nil {
		l.opts.Log.Warn("could not look up account recovery")
		return
	}
	if !found || !recoveryAddress(a.Mail) {
		return
	}
	subject, body := "EA Nation account name", fmt.Sprintf("Your EA Nation account name is %s.\r\n", a.Name)
	var hash string
	if name != "" {
		var secret [6]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return
		}
		password := hex.EncodeToString(secret[:])
		encoded, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return
		}
		hash = string(encoded)
		subject = "Rei Nation password recovery"
		body = fmt.Sprintf("Sign in as %s within 30 minutes using this recovery password: %s\r\n"+
			"After signing in, use Edit Account to choose a new password.\r\n", a.Name, password)
	}
	reserved, err := l.accounts.ReserveRecovery(ctx, a, hash)
	if err != nil {
		l.opts.Log.Warn("could not reserve account recovery")
		return
	}
	if reserved {
		if err := l.opts.SendRecoveryMail(ctx, a.Mail, subject, body); err != nil {
			l.opts.Log.Warn("could not deliver account recovery email")
		}
	}
}

func (l *lobby) useRecoveryPassword(ctx context.Context, account storage.Account, password []byte) bool {
	if l.accounts == nil {
		return false
	}
	hash, err := l.accounts.RecoveryHash(ctx, account.ID)
	if err != nil || hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), password) != nil {
		return false
	}
	used, err := l.accounts.UseRecovery(ctx, account, hash)
	return err == nil && used
}
