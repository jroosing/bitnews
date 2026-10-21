//go:build linux && netns

package netbind_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/netbind"
)

// This test needs root, iproute2 and NETBIND_NETNS=1; see
// docs/network-binding.md. It builds, in a throw-away network namespace
// "nbpeer" that plays the remote host:
//
//	main ns: nbveth0 10.99.0.1/24 <-> nbveth1 10.99.0.3/24 :nbpeer   (the "VPN")
//	main ns: nbveth2 10.98.0.1/24 <-> nbveth3 10.98.0.2/24 :nbpeer   (the "leak" path)
//
// the peer serves on 10.99.0.2/32 (its loopback), and a route over nbveth2 to 10.99.0.0/24 with a worse metric, so that an
// unbound socket would still reach the peer when nbveth0 disappears. A
// bound socket must not.
const (
	peerNS     = "nbpeer"
	vpnIf      = "nbveth0"
	vpnAddr    = "10.99.0.1"
	peerAddr   = "10.99.0.2"
	leakIf     = "nbveth2"
	peerPort   = "9000"
	peerHelper = "NETBIND_NETNS_PEER"
)

func ip(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("ip", args...).CombinedOutput()
	require.NoError(t, err, "ip %s: %s", strings.Join(args, " "), out)
}

func ipPeer(t *testing.T, args ...string) {
	t.Helper()
	ip(t, append([]string{"netns", "exec", peerNS, "ip"}, args...)...)
}

func createVPN(t *testing.T) {
	t.Helper()
	ip(t, "link", "add", vpnIf, "type", "veth", "peer", "name", "nbveth1")
	ip(t, "link", "set", "nbveth1", "netns", peerNS)
	ip(t, "addr", "add", vpnAddr+"/24", "dev", vpnIf)
	ip(t, "link", "set", vpnIf, "up")
	ipPeer(t, "addr", "add", "10.99.0.3/24", "dev", "nbveth1")
	ipPeer(t, "link", "set", "nbveth1", "up")
}

func setupNetns(t *testing.T) {
	t.Helper()
	if os.Getenv("NETBIND_NETNS") != "1" {
		t.Skip("set NETBIND_NETNS=1 (and run as root) to run the network namespace test")
	}
	if os.Geteuid() != 0 {
		t.Skip("the network namespace test needs root")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 (ip) not found")
	}
	_ = exec.Command("ip", "netns", "del", peerNS).Run()
	_ = exec.Command("ip", "link", "del", vpnIf).Run()
	_ = exec.Command("ip", "link", "del", leakIf).Run()
	ip(t, "netns", "add", peerNS)
	t.Cleanup(func() {
		_ = exec.Command("ip", "link", "del", vpnIf).Run()
		_ = exec.Command("ip", "link", "del", leakIf).Run()
		_ = exec.Command("ip", "netns", "del", peerNS).Run()
	})
	ipPeer(t, "link", "set", "lo", "up")
	// The service address lives on the peer's loopback so that it survives
	// the VPN veth being deleted and is reachable over either path.
	ipPeer(t, "addr", "add", peerAddr+"/32", "dev", "lo")
	createVPN(t)
	ip(t, "link", "add", leakIf, "type", "veth", "peer", "name", "nbveth3")
	ip(t, "link", "set", "nbveth3", "netns", peerNS)
	ip(t, "addr", "add", "10.98.0.1/24", "dev", leakIf)
	ip(t, "link", "set", leakIf, "up")
	ipPeer(t, "addr", "add", "10.98.0.2/24", "dev", "nbveth3")
	ipPeer(t, "link", "set", "nbveth3", "up")
	ip(t, "route", "add", "10.99.0.0/24", "via", "10.98.0.2", "dev", leakIf, "metric", "200")

	cmd := exec.Command("ip", "netns", "exec", peerNS, os.Args[0], "-test.run", "^TestNetnsPeerServer$", "-test.timeout", "10m")
	cmd.Env = append(os.Environ(), peerHelper+"=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(peerAddr, peerPort), time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 10*time.Second, 100*time.Millisecond, "the peer server never answered: %s", &out)
}

// TestNetnsPeerServer is the remote host: it runs in the peer namespace as a
// child of the test and accepts connections until killed.
func TestNetnsPeerServer(t *testing.T) {
	if os.Getenv(peerHelper) != "1" {
		t.Skip("helper process of TestNetnsKillSwitch")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(peerAddr, peerPort))
	require.NoError(t, err)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("hello\n"))
		_ = c.Close()
	}
}

func dialBound(b netbind.Binding) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := b.Dialer(nil).DialContext(ctx, "tcp", net.JoinHostPort(peerAddr, peerPort))
	if err != nil {
		return err
	}
	defer c.Close()
	buf := make([]byte, 6)
	_, err = c.Read(buf)
	return err
}

func txPackets(t *testing.T, ifName string) int {
	t.Helper()
	raw, err := os.ReadFile("/sys/class/net/" + ifName + "/statistics/tx_packets")
	require.NoError(t, err)
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	return n
}

func TestNetnsKillSwitch(t *testing.T) {
	setupNetns(t)
	b := netbind.Binding{Interface: vpnIf, Address: vpnAddr}

	var mu sync.Mutex
	var calls []string
	signal := make(chan string, 8)
	record := func(s string) { mu.Lock(); calls = append(calls, s); mu.Unlock(); signal <- s }
	ks := netbind.NewKillswitch(netbind.KillswitchConfig{
		Binding: b, Interval: time.Second,
		Pause:  func(reason string) { record("pause: " + reason) },
		Resume: func() { record("resume") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ks.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// The VPN is up: bound traffic flows and nothing is paused.
	require.NoError(t, dialBound(b), "bound dial over the VPN interface")
	require.NoError(t, dialBound(netbind.Binding{Interface: vpnIf}), "interface-only binding")

	// The VPN goes away. The unbound control dial still gets through the
	// other route, so a bound dial that fails proves the binding is doing it.
	ip(t, "link", "del", vpnIf)
	start := time.Now()
	select {
	case s := <-signal:
		assert.Contains(t, s, "pause", "first call after the interface vanished")
		assert.Contains(t, s, vpnIf)
	case <-time.After(5 * time.Second):
		t.Fatal("the kill switch did not pause within 5 s of the interface disappearing")
	}
	t.Logf("paused %v after the interface was deleted", time.Since(start))

	leakBefore := txPackets(t, leakIf)
	assert.Error(t, dialBound(b), "address+interface binding must not reach the peer over another route")
	assert.Error(t, dialBound(netbind.Binding{Interface: vpnIf}), "interface-only binding")
	assert.Error(t, dialBound(netbind.Binding{Address: vpnAddr}), "address-only binding: the address is gone with the interface")
	assert.Equal(t, leakBefore, txPackets(t, leakIf), "no packet may leave on the other interface")
	c, err := net.DialTimeout("tcp", net.JoinHostPort(peerAddr, peerPort), 2*time.Second)
	require.NoError(t, err, "control: an unbound dial does reach the peer over the other route")
	_ = c.Close()

	// The VPN comes back and the queue resumes.
	createVPN(t)
	select {
	case s := <-signal:
		assert.Equal(t, "resume", s)
	case <-time.After(5 * time.Second):
		t.Fatal("the kill switch did not resume within 5 s of the interface returning")
	}
	require.Eventually(t, func() bool { return dialBound(b) == nil }, 5*time.Second, 200*time.Millisecond,
		"bound dials work again")
	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, calls, 2, fmt.Sprint(calls))
}
