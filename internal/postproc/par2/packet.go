package par2

import "encoding/hex"

// ID is a 16-byte identifier: a file ID or a recovery set ID.
type ID [16]byte

// String returns the ID in lower-case hex.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Packet is one valid packet found in a stream.
type Packet struct {
	SetID  ID    // recovery set ID from the header
	Offset int64 // offset of the packet header in the stream
	Length int64 // total packet length including the header
	Body   Body  // *Main, *FileDesc, *IFSC, *RecvSlic or *Creator
}

// Body is the parsed content of a packet.
type Body interface{ isBody() }

// Main is the Main packet body.
type Main struct {
	SliceSize      uint64
	RecoveryIDs    []ID
	NonRecoveryIDs []ID
	DerivedSetID   ID
}

// FileDesc is the File Description packet body.
type FileDesc struct {
	FileID ID
	MD5    [16]byte
	MD516k [16]byte
	Length uint64
	Name   string
}

// SliceChecksum is one IFSC entry.
type SliceChecksum struct {
	MD5   [16]byte
	CRC32 uint32
}

// IFSC is the Input File Slice Checksum packet body.
type IFSC struct {
	FileID ID
	Slices []SliceChecksum
}

// RecvSlic describes a recovery slice without reading its data.
type RecvSlic struct {
	Exponent   uint32
	DataOffset int64
	DataLength int64
}

// Creator is the Creator packet body.
type Creator struct{ Text string }

func (*Main) isBody()     {}
func (*FileDesc) isBody() {}
func (*IFSC) isBody()     {}
func (*RecvSlic) isBody() {}
func (*Creator) isBody()  {}
