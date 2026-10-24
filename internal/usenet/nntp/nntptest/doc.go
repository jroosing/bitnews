// Package nntptest is a scriptable in-memory NNTP Server for tests above the
// NNTP client: the connection pool, the Article scheduler and the Usenet
// Queue runner. It needs no sockets and no TLS, so those tests stay fast and
// cannot weaken the client's strict TLS (which has no test hook outside
// package nntp).
//
// A Server hands out nntp.ArticleConn values through Factory, which plugs
// straight into nntp.NewPool:
//
//	srv := nntptest.NewServer("main")
//	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": data}, 700_000)
//	fx.Install(srv)
//	pool := nntp.NewPool(nntp.PoolConfig{Name: "main", MaxConnections: 8, Factory: srv.Factory()})
//
// fx.NZB is a document for package nzb, and every message ID in it is served
// by srv as yEnc multipart articles with correct begin, end and CRC32 values.
// Install the same Fixture on a second Server to play a fill Server.
//
// # Scripting
//
// All scripting methods may be called while connections are open.
//
//   - AddArticle stores raw body lines; EncodeArticles makes them from a
//     payload.
//   - Missing makes chosen IDs answer 430 (nntp.ErrArticleNotFound).
//   - Corrupt serves chosen IDs with a flipped data byte, so the yEnc CRC32
//     check fails while the part still decodes to its declared length.
//   - SetLatency delays every BODY and DATE; the wait honors the context.
//   - Disconnect drops the connection after N lines of a body
//     (nntp.ErrConnection, connection dead), always or a number of times.
//   - FailAuth makes every connect fail with nntp.ErrAuth.
//   - SetMaxConnections refuses extra connects like a 502 would
//     (nntp.ErrConnection).
//   - FailNextDials makes the next connects fail with nntp.ErrConnection.
//   - SetRetentionDays only records the retention, for scheduler tests.
//
// # Observing
//
// Requests, TotalRequests, RequestedIDs and ConnRequests count BODY commands
// per message ID and per connection, so a scheduler test can assert that a
// fill Server was never asked for an Article the main Server delivered.
// Dials, AuthAttempts, OpenConns and PeakConns describe connections; a pool
// test asserts its limit with PeakConns.
//
// # Fidelity
//
// The in-memory connections follow nntp.Conn: the sink gets dot-unstuffed
// lines without CRLF in a buffer that is reused between lines (keep a copy);
// a sink error or a cancelled command kills the connection; a dead
// connection and an invalid message ID fail before the Server sees anything.
// The yEnc encoder in EncodeArticles is independent of package yenc.
package nntptest

import "github.com/jroosing/bitnews/internal/usenet/nntp"

// Both the real client and the in-memory one can sit behind a pool.
var (
	_ nntp.ArticleConn = (*nntp.Conn)(nil)
	_ nntp.ArticleConn = (*conn)(nil)
)
