package tidy

import "regexp"

// cleanSteps are the substitutions of Clean.py, in the script's order. Each
// removes one suffix or tag from the end of the name, matching without regard
// to case.
var cleanSteps = func() []*regexp.Regexp {
	patterns := []string{
		`-4P$`,
		`-4Planet$`,
		`-AlternativeToRequested$`,
		`-AlteZachen$`,
		`-AsRequested$`,
		`-AsRequested-xpost$`,
		`-BUYMORE$`,
		`-Chamele0n$`,
		`-GEROV$`,
		`-iNC0GNiTO$`,
		`-NZBGeek$`,
		`-Obfuscated$`,
		`-Obfuscation$`,
		`-postbot$`,
		`-Rakuv[a-z0-9]*$`,
		`-RePACKPOST$`,
		`-Scrambled$`,
		`-WhiteRev$`,
		`-WRTEAM$`,
		`-CAPTCHA$`,
		`-Z0iDS3N$`,
		`\[eztv([ ._-]re)?\]$`,
		`\[TGx\]$`,
		`\[ettv\]$`,
		`\[TGx\]-xpost$`,
		`.mkv-xpost$`, // the dot is a wildcard in the original too
		`-xpost$`,
	}
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(`(?i)` + p)
	}
	return out
}()

// trailingGroup is the script's last step: after a group name (or the
// literal -D-Z0N3), a further "-" and exactly four characters are removed.
var trailingGroup = regexp.MustCompile(`(?i)(-D-Z0N3|-[^-.\n]*)(-.{4})?$`)

// Clean returns the name without the suffixes and tags release bots add, as
// Clean.py does. A name that would become empty is returned unchanged.
func Clean(name string) string {
	s := name
	for _, re := range cleanSteps {
		s = re.ReplaceAllString(s, "")
	}
	s = trailingGroup.ReplaceAllString(s, "${1}")
	if s == "" {
		return name
	}
	return s
}
