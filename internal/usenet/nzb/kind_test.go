package nzb_test

import (
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nzb"
)

func TestParse_ClassifiesFilesByName(t *testing.T) {
	cases := []struct {
		subject string
		kind    nzb.Kind
		blocks  int
	}{
		{`"movie.part01.rar" yEnc (1/50)`, nzb.KindData, 0},
		{`"movie.nfo" yEnc (1/1)`, nzb.KindData, 0},
		{`"movie.par2" yEnc (1/1)`, nzb.KindPar2Index, 0},
		{`"MOVIE.PAR2" yEnc (1/1)`, nzb.KindPar2Index, 0},
		{`"movie.vol00+01.par2" yEnc (1/1)`, nzb.KindPar2Volume, 1},
		{`"movie.vol03+04.par2" yEnc (1/3)`, nzb.KindPar2Volume, 4},
		{`"movie.vol127+128.par2" yEnc (1/90)`, nzb.KindPar2Volume, 128},
		{`"Movie.VOL07+08.PAR2" yEnc (1/5)`, nzb.KindPar2Volume, 8},
		{`"movie.vol00+99999999999999999999.par2" yEnc (1/1)`, nzb.KindPar2Volume, 0}, // MM overflows int
		{`"movie.vol.par2" yEnc (1/1)`, nzb.KindPar2Index, 0},
		{`"movie.par2.rar" yEnc (1/1)`, nzb.KindData, 0},
		{`"a8f3e9c2b1d4e5f6" yEnc (1/1)`, nzb.KindData, 0}, // obfuscated: unknown until yEnc/PAR2 tell
	}
	for _, c := range cases {
		doc := `<nzb><file subject="` + escape(c.subject) + `"><segments><segment number="1">a@x</segment></segments></file></nzb>`
		f := parseString(t, doc).Files[0]
		if f.Kind != c.kind || f.RecoveryBlocks != c.blocks {
			t.Errorf("%q: kind=%v blocks=%d, want kind=%v blocks=%d", c.subject, f.Kind, f.RecoveryBlocks, c.kind, c.blocks)
		}
	}
}
