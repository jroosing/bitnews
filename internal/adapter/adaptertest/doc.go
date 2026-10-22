// Package adaptertest is the replay harness for the media-manager adapter
// contract tests (M4-06, and the qBittorrent fixtures of M5-12). It knows
// nothing about SABnzbd or qBittorrent: it loads fixture files, runs their
// requests against any http.Handler and compares the responses with matchers
// that ignore volatile values. The adapter's own tests seed the state a
// fixture expects (the fixture's seed list is opaque to this package).
//
// A fixture is a JSON file:
//
//	{
//	  "name": "queue lists a downloading slot",
//	  "provenance": "hand-written from the SABnzbd API docs, NOT recorded; ...",
//	  "seed": [ ...adapter-specific entries... ],
//	  "steps": [
//	    {
//	      "request": {"method": "GET", "target": "/sabnzbd/api?mode=queue&apikey={{apikey}}",
//	                  "headers": {...}, "body": "...", "multipart": [{"name":..., "filename":..., "file": "x.nzb"}]},
//	      "response": {"status": 200, "headers": {"Content-Type": "$re:^application/json"},
//	                   "json": {...expected...}},
//	      "capture": {"nzo": "$.nzo_ids[0]"}
//	    }
//	  ]
//	}
//
// {{name}} in a request or in an expected string is replaced by a variable:
// the ones the seeding returns, and the ones earlier steps captured.
//
// Matchers, written as expected string values in "json" (and header values):
//
//	"$any"           the key must exist; any value
//	"$absent"        the key must not exist
//	"$type:T"        T is string, number, bool, object, array or null
//	"$re:PATTERN"    a string that matches the regular expression
//	"$$text"         the literal string "$text"
//
// Anything else must be equal. An expected object may omit keys the response
// has (media managers ignore fields they do not know), unless it contains
// "$exact": true. Arrays must have the same length.
package adaptertest
