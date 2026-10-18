package auth

import "testing"

// FuzzParseHash feeds hostile stored hashes to the parser. It never runs
// argon2, so absurd parameters must be rejected before any hashing.
func FuzzParseHash(f *testing.F) {
	f.Add([]byte("$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA"))
	f.Add([]byte("$argon2id$v=19$m=99999999999,t=1,p=1$c2FsdA$aGFzaA"))
	f.Add([]byte("$argon2id$v=19$m=8,t=1,p=1$$"))
	f.Add([]byte(""))
	f.Add([]byte("$$$$$$"))
	f.Fuzz(func(t *testing.T, hash []byte) {
		ph, err := parseHash(hash)
		if err != nil {
			return
		}
		if ph.Memory > maxMemoryKiB || ph.Time > maxTime || ph.Threads > maxThreads ||
			ph.Memory < 8*uint32(ph.Threads) || ph.Time < 1 || ph.Threads < 1 {
			t.Fatalf("accepted out-of-range params %+v for %q", ph.Params, hash)
		}
		if len(ph.salt) != ph.SaltLen || len(ph.key) != ph.KeyLen {
			t.Fatalf("length mismatch for %q", hash)
		}
	})
}
