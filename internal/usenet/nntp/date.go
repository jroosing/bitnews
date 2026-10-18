package nntp

import (
	"context"
	"time"
)

// dateLayout is the timestamp format of the DATE response (RFC 3977 7.1).
const dateLayout = "20060102150405"

// Date asks the Server for its clock (DATE) and returns it in UTC. It is a
// cheap health check. Any response other than 111 with a valid timestamp
// kills the connection: 4xx and 5xx give ErrConnection, anything else
// ErrProtocol.
func (c *Conn) Date(ctx context.Context) (time.Time, error) {
	stop, err := c.begin(ctx, "date")
	if err != nil {
		return time.Time{}, err
	}
	defer stop()

	code, text, err := c.roundTrip("DATE")
	if err != nil {
		return time.Time{}, c.fail(ctx, "date", err)
	}
	if code != 111 {
		return time.Time{}, c.refuse("date", code, text, classifyUnexpected(code))
	}
	t, ok := parseDate(text)
	if !ok {
		return time.Time{}, c.refuse("date", code, text, ErrProtocol)
	}
	return t, nil
}

func parseDate(s string) (time.Time, bool) {
	if len(s) != len(dateLayout) {
		return time.Time{}, false
	}
	for i := range len(s) {
		if !isDigit(s[i]) {
			return time.Time{}, false
		}
	}
	t, err := time.ParseInLocation(dateLayout, s, time.UTC)
	return t, err == nil
}
