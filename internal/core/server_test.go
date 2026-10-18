package core_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func validServer() core.Server {
	return core.Server{
		ID: core.NewID(), Name: "Newshosting", Host: "news.example.com", Port: 563,
		Username: "joe", Password: "hunter2", MaxConnections: 20, RetentionDays: 0, Priority: 0, Enabled: true,
	}
}

func TestValidServerIsValid(t *testing.T) {
	require.NoError(t, validServer().Validate())
}

func TestServerHostAcceptsHostnamesAndIPs(t *testing.T) {
	for _, host := range []string{"news.example.com", "localhost", "a-b.c", "usenet_1", "10.0.0.5", "::1", "2001:db8::1", "NEWS.Example.COM", strings.Repeat("a", 63) + ".com"} {
		s := validServer()
		s.Host = host
		assert.NoError(t, s.Validate(), host)
	}
}

func TestServerHostRejectsAnythingThatIsNotABareHost(t *testing.T) {
	for _, host := range []string{
		"", " ", "nntps://news.example.com", "news.example.com/path", "news.example.com:563",
		"user@news.example.com", "news example.com", "-bad.example.com", "bad-.example.com",
		"a..b", ".a", "a.", "[::1]", "news.example.com?x=1", "new\x00s", strings.Repeat("a", 64) + ".com",
		strings.Repeat("a.", 130), "ünicode.example.com", "fe80::1%eth0",
	} {
		s := validServer()
		s.Host = host
		assert.ErrorIs(t, s.Validate(), core.ErrInvalidServerHost, "host %q", host)
	}
}

func TestServerFieldLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*core.Server)
		want error
	}{
		"empty name":       {func(s *core.Server) { s.Name = "" }, core.ErrInvalidServerName},
		"blank name":       {func(s *core.Server) { s.Name = "  " }, core.ErrInvalidServerName},
		"long name":        {func(s *core.Server) { s.Name = strings.Repeat("x", 65) }, core.ErrInvalidServerName},
		"control in name":  {func(s *core.Server) { s.Name = "a\nb" }, core.ErrInvalidServerName},
		"port zero":        {func(s *core.Server) { s.Port = 0 }, core.ErrInvalidServerPort},
		"port negative":    {func(s *core.Server) { s.Port = -1 }, core.ErrInvalidServerPort},
		"port too big":     {func(s *core.Server) { s.Port = 65536 }, core.ErrInvalidServerPort},
		"no connections":   {func(s *core.Server) { s.MaxConnections = 0 }, core.ErrInvalidServerMaxConnections},
		"101 connections":  {func(s *core.Server) { s.MaxConnections = 101 }, core.ErrInvalidServerMaxConnections},
		"negative days":    {func(s *core.Server) { s.RetentionDays = -1 }, core.ErrInvalidServerRetention},
		"negative prio":    {func(s *core.Server) { s.Priority = -1 }, core.ErrInvalidServerPriority},
		"crlf in username": {func(s *core.Server) { s.Username = "a\r\nQUIT" }, core.ErrInvalidServerCredentials},
		"nul in password":  {func(s *core.Server) { s.Password = "a\x00b" }, core.ErrInvalidServerCredentials},
		"long username":    {func(s *core.Server) { s.Username = strings.Repeat("u", 257) }, core.ErrInvalidServerCredentials},
	} {
		t.Run(name, func(t *testing.T) {
			s := validServer()
			tc.mut(&s)
			assert.ErrorIs(t, s.Validate(), tc.want)
		})
	}
	for _, edge := range []func(*core.Server){
		func(s *core.Server) { s.Port = 1 },
		func(s *core.Server) { s.Port = 65535 },
		func(s *core.Server) { s.MaxConnections = 1 },
		func(s *core.Server) { s.MaxConnections = 100 },
		func(s *core.Server) { s.RetentionDays = 5000 },
		func(s *core.Server) { s.Username, s.Password = "", "" },
	} {
		s := validServer()
		edge(&s)
		assert.NoError(t, s.Validate())
	}
}

func TestDefaultServerPortIs563(t *testing.T) {
	assert.Equal(t, 563, core.DefaultServerPort)
}

// The data model has no way to weaken TLS: no field for skip-verify, plain
// text or the TLS version, in the struct or in anything it serializes to.
func TestServerHasNoTLSWeakeningField(t *testing.T) {
	typ := reflect.TypeFor[core.Server]()
	for f := range typ.Fields() {
		low := strings.ToLower(f.Name)
		for _, banned := range []string{"tls", "ssl", "insecure", "skip", "verify", "plain", "secure", "starttls"} {
			assert.NotContains(t, low, banned, "field %s", f.Name)
		}
	}
	b, err := json.Marshal(validServer())
	require.NoError(t, err)
	assert.NotContains(t, string(b), "hunter2")
}

func TestServersChangedEventHasItsOwnKind(t *testing.T) {
	var ev core.Event = core.ServersChanged{}
	assert.Equal(t, core.EventKind("servers_changed"), ev.Kind())
	assert.False(t, ev.Kind().IsProgress())
}
