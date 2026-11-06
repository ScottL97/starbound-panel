package supervisor

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// Player is one client currently connected to the game server.
//
// Starbound does not expose a query protocol, so the list is derived from the
// server console: the dedicated server logs a descriptive client name on
// connect/disconnect (vanilla and OpenStarbound both do), and chat lines carry
// the player name.
type Player struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Host  string    `json:"host"`
	Since time.Time `json:"since"`
}

var (
	reConnected    = regexp.MustCompile(`UniverseServer: Client (.+?) connected\s*$`)
	reDisconnected = regexp.MustCompile(`UniverseServer: Client (.+?) disconnected for reason: ?(.*)$`)
	reLost         = regexp.MustCompile(`UniverseServer: Client (.+?) connection lost\s*$`)
	reKick         = regexp.MustCompile(`UniverseServer: Kicking client (.+?) due to inactivity`)
	reChat         = regexp.MustCompile(`Chat: <(.+?)>`)
	reDescriptive  = regexp.MustCompile(`^'(.*)' <([^>]*)> \((.*)\)$`)
)

type parser struct {
	mu       sync.Mutex
	players  map[string]*Player
	order    []string
	onChange func([]Player)
}

func newParser(onChange func([]Player)) *parser {
	return &parser{players: map[string]*Player{}, onChange: onChange}
}

// Feed inspects one console line and updates the player list.
func (p *parser) Feed(text string) {
	p.mu.Lock()
	changed := p.feedLocked(text)
	snap := p.snapshotLocked()
	p.mu.Unlock()

	if changed && p.onChange != nil {
		p.onChange(snap)
	}
}

// Reset clears the list, e.g. when the server is (re)started.
func (p *parser) Reset() {
	p.mu.Lock()
	p.players = map[string]*Player{}
	p.order = nil
	p.mu.Unlock()
}

// Snapshot returns the current players in join order.
func (p *parser) Snapshot() []Player {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

func (p *parser) feedLocked(text string) bool {
	switch {
	case reConnected.MatchString(text):
		name, id, host := splitClient(reConnected.FindStringSubmatch(text)[1])
		return p.addLocked(id, name, host)

	case reDisconnected.MatchString(text):
		return p.removeLocked(splitClient(reDisconnected.FindStringSubmatch(text)[1]))

	case reLost.MatchString(text):
		return p.removeLocked(splitClient(reLost.FindStringSubmatch(text)[1]))

	case reKick.MatchString(text):
		return p.removeLocked(splitClient(reKick.FindStringSubmatch(text)[1]))

	case reChat.MatchString(text):
		// Vanilla logs some clients without a name first; a chat line is the
		// earliest reliable name. Only use it when it is unambiguous.
		return p.nameLocked(reChat.FindStringSubmatch(text)[1])
	}
	return false
}

// splitClient understands both the descriptive form used by Starbound
// ("'Nyazuli' <1> (192.168.1.5)") and a bare client id or uuid.
func splitClient(s string) (name, id, host string) {
	s = strings.TrimSpace(s)
	if m := reDescriptive.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
	}
	return "", s, ""
}

func (p *parser) addLocked(id, name, host string) bool {
	if id == "" {
		id = name
	}
	if id == "" {
		return false
	}
	if existing, ok := p.players[id]; ok {
		changed := false
		if name != "" && existing.Name != name {
			existing.Name = name
			changed = true
		}
		if host != "" && existing.Host != host {
			existing.Host = host
			changed = true
		}
		return changed
	}
	p.players[id] = &Player{ID: id, Name: name, Host: host, Since: time.Now()}
	p.order = append(p.order, id)
	return true
}

func (p *parser) removeLocked(name, id, host string) bool {
	key := id
	if key == "" {
		key = name
	}
	if _, ok := p.players[key]; ok {
		delete(p.players, key)
		p.removeOrderLocked(key)
		return true
	}
	if name == "" {
		return false
	}
	for k, pl := range p.players {
		if pl.Name == name {
			delete(p.players, k)
			p.removeOrderLocked(k)
			return true
		}
	}
	return false
}

func (p *parser) removeOrderLocked(key string) {
	for i, id := range p.order {
		if id == key {
			p.order = append(p.order[:i], p.order[i+1:]...)
			return
		}
	}
}

func (p *parser) nameLocked(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, pl := range p.players {
		if pl.Name == name {
			return false
		}
	}
	// Only assign when exactly one connected client is still unnamed.
	var target *Player
	unnamed := 0
	for _, pl := range p.players {
		if pl.Name == "" {
			unnamed++
			target = pl
		}
	}
	if unnamed != 1 {
		return false
	}
	target.Name = name
	return true
}

func (p *parser) snapshotLocked() []Player {
	out := make([]Player, 0, len(p.order))
	for _, id := range p.order {
		if pl, ok := p.players[id]; ok {
			out = append(out, *pl)
		}
	}
	return out
}
