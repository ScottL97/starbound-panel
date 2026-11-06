package supervisor

import "testing"

func TestParserTracksConnectedPlayers(t *testing.T) {
	var snapshots [][]Player
	p := newParser(func(ps []Player) { snapshots = append(snapshots, ps) })

	p.Feed("[15:02:01.503] [Info] UniverseServer: Client 'Neon' <1> (192.168.1.5) connected")
	p.Feed("[15:02:05.000] [Info] Chat: <Neon> hello there")

	players := p.Snapshot()
	if len(players) != 1 {
		t.Fatalf("expected 1 player, got %d", len(players))
	}
	if players[0].Name != "Neon" || players[0].ID != "1" || players[0].Host != "192.168.1.5" {
		t.Fatalf("unexpected player: %+v", players[0])
	}

	p.Feed("[15:03:00.000] [Info] UniverseServer: Client 'Neon' <1> (192.168.1.5) disconnected for reason: Client left")
	if len(p.Snapshot()) != 0 {
		t.Fatalf("player was not removed: %+v", p.Snapshot())
	}
	if len(snapshots) < 2 {
		t.Fatalf("expected change callbacks, got %d", len(snapshots))
	}
}

func TestParserHandlesVanillaBareClientID(t *testing.T) {
	p := newParser(nil)

	p.Feed("[Info] UniverseServer: Client 6f1c0d3a connected")
	players := p.Snapshot()
	if len(players) != 1 || players[0].ID != "6f1c0d3a" || players[0].Name != "" {
		t.Fatalf("unexpected players: %+v", players)
	}

	// A single unnamed client is named by its first chat line.
	p.Feed("[Info] Chat: <Steve> hi")
	players = p.Snapshot()
	if players[0].Name != "Steve" {
		t.Fatalf("chat name was not applied: %+v", players[0])
	}

	p.Feed("[Info] UniverseServer: Client 6f1c0d3a connection lost")
	if len(p.Snapshot()) != 0 {
		t.Fatalf("connection lost did not remove the player")
	}
}

func TestParserKickAndMultipleClients(t *testing.T) {
	p := newParser(nil)

	p.Feed("[Info] UniverseServer: Client 'A' <1> (10.0.0.1) connected")
	p.Feed("[Info] UniverseServer: Client 'B' <2> (10.0.0.2) connected")
	if len(p.Snapshot()) != 2 {
		t.Fatalf("expected 2 players")
	}

	// Two unnamed clients: a chat line must not guess.
	p.Feed("[Info] UniverseServer: Client 3 connected")
	p.Feed("[Info] UniverseServer: Client 4 connected")
	p.Feed("[Info] Chat: <C> hey")
	for _, pl := range p.Snapshot() {
		if pl.Name == "C" {
			t.Fatalf("ambiguous chat name should not be assigned")
		}
	}

	p.Feed("[Info] UniverseServer: Kicking client 'A' <1> (10.0.0.1) due to inactivity")
	for _, pl := range p.Snapshot() {
		if pl.ID == "1" {
			t.Fatalf("kicked client still listed")
		}
	}
}

func TestParserReset(t *testing.T) {
	p := newParser(nil)
	p.Feed("[Info] UniverseServer: Client 'A' <1> (10.0.0.1) connected")
	p.Reset()
	if len(p.Snapshot()) != 0 {
		t.Fatalf("reset did not clear players")
	}
}
