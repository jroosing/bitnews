package nzb_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jroosing/bitnews/internal/usenet/nzb"
)

const billionLaughsDTD = `<?xml version="1.0"?>
<!DOCTYPE nzb [
 <!ENTITY lol "lol">
 <!ENTITY lol1 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">
 <!ENTITY lol2 "&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;">
 <!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">
 <!ENTITY lol4 "&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;">
 <!ENTITY lol5 "&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;">
 <!ENTITY lol6 "&lol5;&lol5;&lol5;&lol5;&lol5;&lol5;&lol5;&lol5;&lol5;&lol5;">
 <!ENTITY lol7 "&lol6;&lol6;&lol6;&lol6;&lol6;&lol6;&lol6;&lol6;&lol6;&lol6;">
 <!ENTITY lol8 "&lol7;&lol7;&lol7;&lol7;&lol7;&lol7;&lol7;&lol7;&lol7;&lol7;">
 <!ENTITY lol9 "&lol8;&lol8;&lol8;&lol8;&lol8;&lol8;&lol8;&lol8;&lol8;&lol8;">
]>
`

const validBody = `<file subject="s"><segments><segment number="1">a@x</segment></segments></file>`

func TestParse_BillionLaughsReferencedIsRejectedWithoutExpansion(t *testing.T) {
	cases := map[string]string{
		"in text":      `<nzb><head><meta type="name">&lol9;</meta></head>` + validBody + `</nzb>`,
		"in attribute": `<nzb><file subject="&lol9;"><segments><segment number="1">a@x</segment></segments></file></nzb>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseErr(t, billionLaughsDTD+body, nzb.DefaultLimits())
			if !errors.Is(err, nzb.ErrMalformed) {
				t.Errorf("err = %v, want ErrMalformed", err)
			}
			if strings.Contains(err.Error(), "lollol") {
				t.Errorf("error text shows expansion: %.200s", err)
			}
		})
	}
}

func TestParse_DTDEntityDefinitionsAreIgnored(t *testing.T) {
	n := parseString(t, billionLaughsDTD+`<nzb><head><meta type="name">plain</meta></head>`+validBody+`</nzb>`)
	if n.Meta.Name != "plain" {
		t.Errorf("meta name = %q, want plain", n.Meta.Name)
	}
}

func TestParse_ExternalEntityIsNotRead(t *testing.T) {
	const secret = "TOP-SECRET-CONTENT-7f3a"
	path := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	dtd := `<?xml version="1.0"?>
<!DOCTYPE nzb [ <!ENTITY xxe SYSTEM "file://` + filepath.ToSlash(path) + `"> ]>
`
	t.Run("referenced", func(t *testing.T) {
		doc := dtd + `<nzb><head><meta type="password">&xxe;</meta></head>` + validBody + `</nzb>`
		err := parseErr(t, doc, nzb.DefaultLimits())
		if !errors.Is(err, nzb.ErrMalformed) {
			t.Errorf("err = %v, want ErrMalformed", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks file content: %v", err)
		}
	})
	t.Run("unreferenced", func(t *testing.T) {
		n := parseString(t, dtd+`<nzb><head><meta type="password">pw</meta></head>`+validBody+`</nzb>`)
		if n.Meta.Password != "pw" || strings.Contains(n.Files[0].Subject, secret) {
			t.Errorf("unexpected result: %+v", n)
		}
	})
}
