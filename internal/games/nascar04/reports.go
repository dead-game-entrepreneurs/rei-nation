package nascar04

import (
	"context"
	"strings"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
)

func (l *lobby) reportPlayer(ctx context.Context, p *player, field string) {
	l.mu.Lock()
	source, account := p.persona, p.account.ID
	live := source != "" && l.online[strings.ToLower(source)] == p
	l.mu.Unlock()
	if !live {
		p.client.Send(aries.Reply("reptmaut", nil))
		return
	}
	defer p.client.Send(aries.Reply("rept", nil))
	target := nameValue(field)
	if l.reports == nil || !personaName.MatchString(target) {
		return
	}
	canonical, err := l.reports.ReportPlayer(ctx, gameKey, account, source, target)
	if err != nil {
		l.opts.Log.Warn("could not save NASCAR player report")
		return
	}
	if canonical == "" {
		return
	}
	reputation, err := l.reports.Reputation(ctx, gameKey, canonical)
	if err != nil {
		l.opts.Log.Warn("could not read NASCAR player reputation")
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if to := l.online[strings.ToLower(canonical)]; to != nil && reputation < to.reputation {
		to.reputation = reputation
		sendPlayerState(to)
		if to.room != nil {
			for viewer := range to.room.members {
				sendRoomMember(viewer, to)
			}
		}
	}
}
