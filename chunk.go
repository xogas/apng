package apng

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

// pngHeader is the 8-byte magic number that begins every PNG file.
var pngHeader = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

// Chunk type identifiers used throughout the encoder and decoder.
const (
	chunkIHDR = "IHDR"
	chunkIDAT = "IDAT"
	chunkIEND = "IEND"
	chunkACTL = "acTL"
	chunkFCTL = "fcTL"
	chunkFDAT = "fdAT"
)

// rawFrame is a direct mapping o the fcTL chunk binary fields.
// Field type match the APNG specification exactly to allow zero-copy deserialization.
// It is only used internally by the decoder; never exposed to callers.
type rawFrame struct {
	seqNum             uint32
	width, height      uint32
	xOffset, yOffset   uint32
	delayNum, delayDen uint16
	dispOp             uint8
	blendOp            uint8
}

// readChunk reads one PNG chunk from r and returns its type name and data payload.
// The CRC-32 checksum is covering the type and data fields is  verified before returning.
// Returns io.EOF when the stream is exhausted.
// Returns a wrapped ErrCRCMismatch error on checksum failure.
func readChunk(r io.Reader) (typ string, data []byte, err error) {
	var length uint32
	if err = binary.Read(r, binary.BigEndian, &length); err != nil {
		return
	}
	h := crc32.NewIEEE()
	tr := io.TeeReader(r, h)

	typBuf := make([]byte, 4)
	if _, err = io.ReadFull(tr, typBuf); err != nil {
		return
	}
	typ = string(typBuf)
	data = make([]byte, length)
	if _, err = io.ReadFull(tr, data); err != nil {
		return
	}

	var crcVal uint32
	if err = binary.Read(r, binary.BigEndian, &crcVal); err != nil {
		return
	}
	if h.Sum32() != crcVal {
		err = errors.New("apng: invalid CRC")
	}
	return
}

// writeChunk writes a complete PNG chunk to w: length (4 bytes) + type (4 bytes) +
// data (n bytes) + CRC-32 checksum (4 bytes) covering the type and data fields.
func writeChunk(w io.Writer, typ string, data []byte) error {
	h := crc32.NewIEEE()
	mw := io.MultiWriter(w, h)

	if err := binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	if _, err := mw.Write([]byte(typ)); err != nil {
		return err
	}

	if _, err := mw.Write(data); err != nil {
		return err
	}

	return binary.Write(w, binary.BigEndian, h.Sum32())
}
