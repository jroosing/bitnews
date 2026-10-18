package core

import "log/slog"

// Redacted is what a Secret shows in place of its value.
const Redacted = "[redacted]"

// Secret holds a value that must never reach logs or API responses, such as a
// Server password. Every default formatting path (fmt, encoding/json, slog)
// shows Redacted instead of the value; Reveal is the only way to read it.
type Secret string

// String implements fmt.Stringer.
func (Secret) String() string { return Redacted }

// GoString implements fmt.GoStringer, which %#v uses.
func (Secret) GoString() string { return Redacted }

// Reveal returns the secret value. Call it only where the value is used, for
// example to authenticate to a Server.
func (s Secret) Reveal() string { return string(s) }

// MarshalJSON implements json.Marshaler. A Secret never serializes its value.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler, which other encoders (and
// text/template-style callers) use. It never returns the value either.
func (Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// LogValue implements slog.LogValuer, so any slog handler logs Redacted.
func (Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }
