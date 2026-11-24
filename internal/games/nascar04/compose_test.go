package nascar04

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

func TestRankAfterReconnect(t *testing.T) {
	addr, dsn := os.Getenv("REI_NATION_TEST_ADDR"), os.Getenv("REI_NATION_TEST_DATABASE_URL")
	if addr == "" || dsn == "" {
		t.Skip("set REI_NATION_TEST_ADDR and REI_NATION_TEST_DATABASE_URL for the isolated stack")
	}
	store, err := storage.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	suffix := testPersonaSuffix()
	relayAddr := os.Getenv("REI_NATION_TEST_RELAY_ADDR")
	var sources [2]string
	if relayAddr != "" {
		sources = [2]string{"127.0.0.4", "127.0.0.5"}
	}
	login := func(name, source string, create bool) (testClient, string) {
		c := dialFrom(t, addr, source)
		c.send("skey", nil)
		c.expect("skey", nil)
		if create {
			c.send("acct", map[string]string{"NAME": name, "PASS": testPassword})
			c.expect("acct", nil)
		}
		c.send("auth", map[string]string{"NAME": name, "PASS": testPassword})
		c.expect("auth", map[string]string{"NAME": name})
		if create {
			c.send("cper", map[string]string{"PERS": name})
			c.expect("cper", nil)
		}
		c.send("pers", map[string]string{"PERS": name})
		reply := c.expect("pers", map[string]string{"PERS": name})
		return c, reply.Fields()["LKEY"]
	}
	a, b, other := "H"+suffix, "J"+suffix, "K"+suffix
	host, hostKey := login(a, sources[0], true)
	guest, guestKey := login(b, sources[1], true)
	buddyAddr := os.Getenv("REI_NATION_TEST_BUDDY_ADDR")
	var hostBuddy, guestBuddy testClient
	if buddyAddr != "" {
		hostBuddy, guestBuddy = dialFrom(t, buddyAddr, sources[0]), dialFrom(t, buddyAddr, sources[1])
		for i, c := range []testClient{hostBuddy, guestBuddy} {
			c.send("AUTH", map[string]string{"LKEY": []string{hostKey, guestKey}[i]})
			c.expect("AUTH", nil)
			c.syncMessengerRoster()
		}
	}
	room := "C." + suffix
	host.send("room", map[string]string{"NAME": room})
	host.expect("room", nil)
	guest.send("move", map[string]string{"NAME": room})
	guest.expect("move", map[string]string{"COUNT": "2"})
	host.send("chal", map[string]string{"PERS": b, "HOST": "1"})
	host.expect("chal", nil)
	guest.send("chal", map[string]string{"PERS": a, "HOST": "0"})
	guest.expect("chal", nil)
	session := host.expect("+ses", map[string]string{"SELF": a, "OPPO": b}).Fields()
	guest.expect("+ses", map[string]string{"WHEN": session["WHEN"]})
	when, err := strconv.ParseInt(session["WHEN"], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	previousHost := host
	guest.c.Close()
	var newHostKey string
	host, newHostKey = login(a, sources[0], false)
	guest, guestKey = login(b, sources[1], false)
	previousHost.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.Copy(io.Discard, previousHost.c); err != nil {
		t.Fatalf("replaced lobby connection stayed open: %v", err)
	}
	if buddyAddr != "" {
		// Both existing Messenger sockets still belong to the authenticated drivers.
		hostBuddy.send("PSET", map[string]string{"SHOW": "CHAT"})
		hostBuddy.expect("PSET", nil)
		hostBuddy.send("AUTH", map[string]string{"LKEY": newHostKey})
		hostBuddy.expect("AUTH", nil)
		hostBuddy.send("SEND", map[string]string{"USER": b, "BODY": "back"})
		hostBuddy.expect("SEND", nil)
		guestBuddy.expect("RECV", map[string]string{"USER": a, "TYPE": "C", "BODY": "back"})
		stale := dialFrom(t, buddyAddr, sources[0])
		stale.send("AUTH", map[string]string{"LKEY": hostKey})
		stale.expect("AUTHfail", nil)
	}
	var verifyRelay func()
	if relayAddr != "" {
		verifyRelay = testPeerServices(t, relayAddr, a, sources)
	}
	unmatched, otherKey := login(other, sources[0], true)

	result := map[string]string{
		"SET_RANKED": "1", "WHEN": time.Unix(when, 0).UTC().Format("2006.1.2 15:04:05"),
		"REPT": a, "NAME0": a, "NAME1": b, "RACETIME": "266001", "SET_TRACKID": "13",
		"FINISPOS0": "1", "FINISPOS1": "37", "LAPSCOMP0": "5", "LAPSCOMP1": "5", "LAPSLED0": "3",
	}
	unmatched.send("rank", result)
	unmatched.expect("rank", nil)
	unmatched.send("snap", map[string]string{"CHAN": "4", "FIND": a})
	unmatched.expect("snap", map[string]string{"RANGE": "0"})

	retailWhen := result["WHEN"]
	result["WHEN"] = strconv.FormatInt(when-1, 10)
	host.send("rank", result)
	host.expect("rank", nil)
	host.send("snap", map[string]string{"CHAN": "4"})
	host.expect("snap", map[string]string{"RANGE": "0"})

	result["WHEN"] = retailWhen
	host.send("rank", result)
	host.expect("rank", nil)
	host.expect("+who", map[string]string{"N": a, "S": ",1,0,,0,,,,,"})
	guest.expect("+who", map[string]string{"N": b, "S": ",0,1,,0,,,,,"})
	if verifyRelay != nil {
		verifyRelay()
	}
	result["REPT"], result["RACETIME"] = b, "266002"
	guest.send("rank", result)
	guest.expect("rank", nil)
	guest.send("snap", map[string]string{"CHAN": "4"})
	guest.expect("snap", map[string]string{"RANGE": "1"})
	guest.expect("+snp", map[string]string{"N": b})

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for i, name := range []string{a, b} {
		rows, err := store.RaceStats(ctx, gameKey, "", name, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Starts != 1 || rows[0].Wins != int64(1-i) || rows[0].Losses != int64(i) {
			t.Fatalf("%s standings after two reports: %+v", name, rows)
		}
	}

	if buddyAddr != "" {
		// An authenticated player behind the same address keeps both sockets.
		otherBuddy := dialFrom(t, buddyAddr, sources[0])
		otherBuddy.send("AUTH", map[string]string{"LKEY": otherKey})
		otherBuddy.expect("AUTH", nil)
		guest.send("sele", map[string]string{"ROOMS": "1"})
		guest.expect("sele", nil)
		otherBuddy.send("RADD", map[string]string{"USER": b, "LIST": "B"})
		otherBuddy.expect("RADD", nil)
		otherBuddy.expect("PGET", map[string]string{"USER": b, "SHOW": "CHAT"})
		guestBuddy.c.Close()
		otherBuddy.expect("PGET", map[string]string{"USER": b, "SHOW": "CHAT", "PROD": lobbyPresenceProduct})
		guestBuddy = dialFrom(t, buddyAddr, sources[1])
		guestBuddy.send("AUTH", map[string]string{"LKEY": guestKey, "PROD": "NASCAR"})
		guestBuddy.expect("AUTH", nil)
		otherBuddy.expect("PGET", map[string]string{"USER": b, "SHOW": "CHAT", "PROD": "NASCAR"})

		browser, _ := login("V"+suffix, sources[0], true)
		browser.send("news", map[string]string{"NAME": "0"})
		browser.expect("newsnew0", nil)
		browser.send("sele", map[string]string{"ROOMS": "1"})
		browser.expect("sele", nil)
		browser.send("peek", map[string]string{"NAME": "E.Open Race"})
		browser.expect("peek", map[string]string{"COUNT": "0"})

		hostBuddy.c.Close()
		stuck := dialFrom(t, buddyAddr, sources[0])
		stuck.send("PSET", map[string]string{"SHOW": "CHAT"})
		stuck.expect("PSETmaut", nil)
		stuck.send("PING", nil)
		host.send("sele", map[string]string{"ROOMS": "1"})
		host.expect("sele", nil)

		now := time.Now()
		stuck.c.SetReadDeadline(now.Add(messengerAuthGrace + 6*time.Second))
		host.c.SetReadDeadline(now.Add(messengerRecoveryGrace + 6*time.Second))
		if _, err := io.Copy(io.Discard, stuck.c); err != nil {
			t.Fatalf("unauthenticated Messenger connection stayed open: %v", err)
		}
		if _, err := io.Copy(io.Discard, host.c); err != nil {
			t.Fatalf("affected lobby connection stayed open: %v", err)
		}
		for _, c := range []testClient{guest, unmatched} {
			c.send("@tic", nil)
			c.expect("@tic", nil)
		}
		browser.send("peek", map[string]string{"NAME": "W.Open Race"})
		browser.expect("peek", map[string]string{"COUNT": "0", "NAME": "W.Open Race"})
		otherBuddy.send("EPGT", map[string]string{"ID": "1"})
		otherBuddy.expect("EPGT", nil)
		guestBuddy.send("SEND", map[string]string{"USER": a, "BODY": "queued during reset"})
		guestBuddy.expect("SEND", nil)

		host, newHostKey = login(a, sources[0], false)
		hostBuddy = dialFrom(t, buddyAddr, sources[0])
		hostBuddy.send("AUTH", map[string]string{"LKEY": newHostKey})
		hostBuddy.expect("AUTH", nil)
		hostBuddy.syncMessengerRoster()
		hostBuddy.expect("RECV", map[string]string{"USER": b, "BODY": "queued during reset"})
		host.send("sele", map[string]string{"ROOMS": "1"})
		host.expect("sele", nil)

		guestBuddy.send("DISC", nil)
		otherBuddy.expect("PGET", map[string]string{"USER": b, "SHOW": "CHAT", "PROD": lobbyPresenceProduct})
		guest.c.Close()
		otherBuddy.expect("PGET", map[string]string{"USER": b, "SHOW": "DISC"})
		login(b, sources[1], false)
		otherBuddy.expect("PGET", map[string]string{"USER": b, "SHOW": "CHAT", "PROD": lobbyPresenceProduct})
	}
}

func testPeerServices(t *testing.T, server, hostName string, source [2]string) func() {
	t.Helper()
	var udp [2]*net.UDPConn
	var observed [2]string
	for i, ip := range source {
		s, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		udp[i] = s
		observed[i] = strconv.Itoa(s.LocalAddr().(*net.UDPAddr).Port)
	}
	dest := func(port uint16) netip.AddrPort {
		return netip.AddrPortFrom(netip.MustParseAddr(server), port)
	}
	getPeer := func(i int) string {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(source[i])}}
		transport := &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		client := &http.Client{Timeout: 2 * time.Second, Transport: transport}
		values := url.Values{"myIP": {source[i]}, "myPort": {"3658"}, "version": {"1.0"}}
		req, err := http.NewRequest("GET", "http://"+dest(guestPort).String()+"/getPeerAddress?"+values.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "sessionID", Value: hostName})
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	send := func(i int, port uint16, text string) {
		if _, err := udp[i].WriteToUDPAddrPort([]byte(text), dest(port)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range source {
		probe := getPeer(i)
		_, tag, ok := strings.Cut(probe, "tag-0=")
		if !ok {
			t.Fatalf("no tag in %q", probe)
		}
		tag, _, _ = strings.Cut(tag, "\n")
		send(i, guestPort, "sourceIP="+source[i]+"\r\nsourcePort=3658\r\ntag="+tag+"\r\n")
	}
	for i := range source {
		want := "status=success\npeerIP=" + source[1-i] + "\npeerPort=" + observed[1-i] + "\n"
		deadline := time.Now().Add(time.Second)
		for {
			if got := getPeer(i); got == want {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("peer mapping: got %q, want %q", got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	checkRoute := func(a, b uint16, phase int) {
		send(0, a, "host first")
		send(1, b, "guest first")
		// Either socket can process the first packet first. Repeat both until the
		// relay has learned both sides, like the native game's probes.
		for i := range source {
			receivePort, sendPort, from := a, b, "guest"
			if i == 1 {
				receivePort, sendPort, from = b, a, "host"
			}
			want := from + " payload " + strconv.Itoa(int(receivePort)) + "/" + strconv.Itoa(phase)
			var data [32]byte
			deadline := time.Now().Add(time.Second)
			for {
				send(1-i, sendPort, want)
				udp[i].SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				n, sender, err := udp[i].ReadFromUDPAddrPort(data[:])
				if err == nil && string(data[:n]) == want {
					if sender != dest(receivePort) {
						t.Fatalf("relay source=%s expected %s", sender, dest(receivePort))
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("relay did not forward %q", want)
				}
			}
		}
	}
	phase := 0
	verifyRelay := func() {
		phase++
		checkRoute(hostPort, guestPort, phase)
		checkRoute(peerPort, peerPort, phase)
	}
	verifyRelay()
	return verifyRelay
}
