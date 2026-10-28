package par2

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Real output of par2cmdline-turbo 1.5.0 (progress lines cut).
const (
	outDamaged = "Loaded 6 new packets\r\nLoaded 4 new packets including 4 recovery blocks\r\n" +
		"There are a total of 17 data blocks.\r\n" +
		"Target: \"b.bin\" - damaged. Found 5 of 7 data blocks.\r\n\r\n" +
		"Repair is required.\r\n2 file(s) exist but are damaged.\r\n" +
		"You have 14 out of 17 data blocks available.\r\nYou have 8 recovery blocks available.\r\n" +
		"Repair is possible.\r\nYou have an excess of 5 recovery blocks.\r\n3 recovery blocks will be used to repair.\r\n"
	outImpossible = "Repair is required.\n1 file(s) are missing.\nYou have 7 out of 17 data blocks available.\n" +
		"You have 8 recovery blocks available.\nRepair is not possible.\nYou need 2 more recovery blocks to be able to repair.\n"
	outClean = "Loaded 4 new packets including 4 recovery blocks\nLoaded 4 new packets including 4 recovery blocks\n" +
		"There are a total of 17 data blocks.\nAll files are correct, repair is not required.\n"
	outRenamed = "File: \"x1f3\" - is a match for \"a.bin\".\n\nRepair is required.\n1 file(s) have the wrong name.\n" +
		"You have 17 out of 17 data blocks available.\nRepair is possible.\n"
)

func TestParseOutput(t *testing.T) {
	t.Run("damaged", func(t *testing.T) {
		o := parseOutput([]byte(outDamaged))
		assert.Equal(t, 3, o.damaged())
		assert.Equal(t, 8, o.recoveryBlocks())
		assert.Equal(t, 3, o.willUse)
	})
	t.Run("impossible", func(t *testing.T) {
		o := parseOutput([]byte(outImpossible))
		assert.Equal(t, 10, o.damaged())
		assert.Equal(t, 8, o.recoveryBlocks())
		assert.Equal(t, 2, o.needMore)
	})
	t.Run("clean takes the recovery blocks from the loaded files", func(t *testing.T) {
		o := parseOutput([]byte(outClean))
		assert.Equal(t, 8, o.recoveryBlocks())
		assert.Zero(t, o.damaged())
	})
	t.Run("renamed", func(t *testing.T) {
		o := parseOutput([]byte(outRenamed))
		assert.Zero(t, o.damaged())
	})
	t.Run("no recovery blocks line means none", func(t *testing.T) {
		o := parseOutput([]byte("You have 16 out of 17 data blocks available.\nRepair is not possible.\n"))
		assert.Equal(t, 1, o.damaged())
		assert.Zero(t, o.recoveryBlocks())
	})
	t.Run("a huge number saturates instead of failing", func(t *testing.T) {
		o := parseOutput([]byte("You have 0 recovery blocks available.\nYou have 99999999999999999999 out of 99999999999999999999 data blocks available.\n"))
		assert.Zero(t, o.damaged())
	})
	t.Run("a file name cannot fake a line", func(t *testing.T) {
		// Names come first on their line, after a fixed word and a quote.
		o := parseOutput([]byte("Target: \"x\" - missing.\nTarget: \"You have 1 out of 1 data blocks available.\" - found.\n"))
		assert.False(t, o.haveAvail)
	})
	t.Run("partial matches", func(t *testing.T) {
		o := parseOutput([]byte("File: \"x7f3a9\" - found 9 of 10 data blocks from \"a.bin\".\nFile: \"junk.dat\" - no data found.\n"))
		assert.Equal(t, []string{"x7f3a9"}, o.partials)
	})
}
