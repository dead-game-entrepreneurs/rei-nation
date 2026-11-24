package nascar04

import (
	"strings"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
)

const raceOfferTimeout = 2 * time.Minute

type raceOffer struct {
	from, to *player
	deadline time.Time
	timer    *time.Timer
}

// Challenge offers and the subsequent chal registration are separate stages.
// Offer, registration and timer changes run under the lobby lock.
func (l *lobby) updateRaceOffer(from, to *player, text string) {
	switch text {
	case "DECL", "BLOC":
		from.clearChallengeWith(to)
	case "ACPT":
		if offer := from.offer; offer != nil && offer == to.offer {
			if deadline := time.Now().Add(30 * time.Second); deadline.After(offer.deadline) {
				offer.deadline = deadline
			}
		}
	default:
		if from.offer != nil || to.offer != nil {
			return
		}
		offer := &raceOffer{from: from, to: to, deadline: time.Now().Add(raceOfferTimeout)}
		from.offer, to.offer = offer, offer
		offer.timer = time.AfterFunc(raceOfferTimeout, func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if from.offer != offer {
				return
			}
			if remaining := time.Until(offer.deadline); remaining > 0 {
				offer.timer.Reset(remaining)
				return
			}
			from.clearChallengeWith(to)
			l.notifyChallengeDeclined(from, to)
			l.notifyChallengeDeclined(to, from)
		})
	}
}

func (l *lobby) cancelChallenge(p *player) {
	peer := p.pending.to
	if offer := p.offer; offer != nil {
		peer = offer.from
		if p == peer {
			peer = offer.to
		}
	}
	if peer != nil {
		p.clearChallengeWith(peer)
		l.notifyChallengeDeclined(peer, p)
	}
	p.pending = challenge{}
}

func (p *player) clearChallengeWith(peer *player) {
	if offer := p.offer; offer != nil && offer == peer.offer {
		offer.timer.Stop()
		p.offer, peer.offer = nil, nil
	}
	if p.pending.to == peer {
		p.pending = challenge{}
	}
	if peer.pending.to == p {
		peer.pending = challenge{}
	}
}

func (l *lobby) notifyChallengeDeclined(to, from *player) {
	if to.lkey != "" && l.online[strings.ToLower(to.persona)] == to {
		sendPrivateMessage(to, from.persona, "DECL", "3")
	}
}

func sendPrivateMessage(to *player, from, text, attr string) {
	to.client.Send(aries.TabReply("+msg", map[string]string{"T": text, "N": from, "U": "", "F": attr + "P"}))
}
