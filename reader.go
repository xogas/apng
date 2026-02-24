package apng

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
)

// decodeState represents the current state of the APNG decoding machine.
type decodeState byte

const (
	// stateHaveIHDR: IHDR parsed, waiting for acTL or IDAT.
	stateHaveIHDR decodeState = iota
	// stateDefaultImage: collecting IDAT for the default image (before first fcTL).
	stateDefaultImage
	// stateInFrame: inside an animation frame, collecting IDAT/fsAT.
	stateInFrame
	// stateDone: IEND reached, decoding complete.
	stateDone
)

// decoder decodes an APNG from a PNG stream.
type decoder struct {
	r    io.Reader
	apng *APNG
	ihdr *ihdrData

	state       decodeState
	idatBuffs   [][]byte
	curRawFrame *rawFrame
}

type ihdrData struct {
	width, height               uint32
	bitDepth, colorType         uint8
	compress, filter, interlace uint8
}

// newDecoder reads and validates the PNG signature, returning an initialized decoder.
func newDecoder(r io.Reader) (*decoder, error) {
	sig := make([]byte, 8)
	if _, err := io.ReadFull(r, sig); err != nil {
		return nil, fmt.Errorf("apng: read PNG signature: %w", err)
	}
	if !bytes.Equal(sig, pngHeader) {
		return nil, ErrInvalidSignature
	}
	return &decoder{r: r}, nil
}

// decode drives the main chunk-processing loop and returns the completed APNG.
func (d *decoder) decode() (*APNG, error) {
loop:
	for {
		typ, data, err := readChunk(d.r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("apng: read chunk: %w", err)
		}

		switch typ {
		case "IHDR":
			if err := d.parseIHDR(data); err != nil {
				return nil, fmt.Errorf("apng: parse IHDR: %w", err)
			}
		case "acTL":
			if err := d.parseACTL(data); err != nil {
				return nil, fmt.Errorf("apng: parse acTL: %w", err)
			}
		case "fcTL":
			if err := d.parseFCTL(data); err != nil {
				return nil, fmt.Errorf("apng: parse fcTL: %w", err)
			}
		case "IDAT":
			if err := d.parseIDAT(data); err != nil {
				return nil, fmt.Errorf("apng: parse IDAT: %w", err)
			}
		case "fdAT":
			if err := d.parseFDAT(data); err != nil {
				return nil, fmt.Errorf("apng: parse fdAT: %w", err)
			}
		case "IEND":
			if err := d.parseIEND(); err != nil {
				return nil, fmt.Errorf("apng: parse IEND: %w", err)
			}
			// Successfully reached the end of the PNG stream.
			break loop
		default:
			// Unknown chunks are harmless; ignore per PNG spec.
		}
	}

	if d.apng == nil {
		return nil, errors.New("apng: missing acTL chunk")
	}
	return d.apng, nil
}

// parseACTL parses the acTL chunk (expects exactly 8 bytes).
// Initializes d.apng and copies canvas dimensions from d.ihdr.
// Returns ErrInvalidChunk if the length is wrong or a second acTL is found.
func (d *decoder) parseIHDR(data []byte) error {
	if len(data) != 13 {
		return fmt.Errorf("%w: IHDR length must be 13, got %d", ErrInvalidChunk, len(data))
	}
	if d.ihdr != nil {
		return fmt.Errorf("%w: multiple IHDR chunks found", ErrInvalidChunk)
	}
	d.ihdr = &ihdrData{
		width:     binary.BigEndian.Uint32(data[0:4]),
		height:    binary.BigEndian.Uint32(data[4:8]),
		bitDepth:  data[8],
		colorType: data[9],
		compress:  data[10],
		filter:    data[11],
		interlace: data[12],
	}
	d.state = stateHaveIHDR
	return nil
}

// parseACTL pareses the acTL chunk (expects exactly 8 bytes).
// Initializes d.apng and copies canvas dimensions from d.ihdr.
// Returns ErrInvalidChunk id the length is wrong or a second acTL is found.
func (d *decoder) parseACTL(data []byte) error {
	if len(data) != 8 {
		return fmt.Errorf("%w: acTL length must be 8, got %d", ErrInvalidChunk, len(data))
	}
	if d.apng != nil {
		return fmt.Errorf("%w: multiple acTL chunks found", ErrInvalidChunk)
	}
	numFrames := binary.BigEndian.Uint32(data[0:4])
	numPlays := binary.BigEndian.Uint32(data[4:8])
	d.apng = &APNG{
		Frames:    make([]Frame, 0, numFrames),
		LoopCount: numPlays,
	}

	// IHDR always precedes acTL per spec; copy canvas size now.
	if d.ihdr != nil {
		d.apng.Width = d.ihdr.width
		d.apng.Height = d.ihdr.height
	}
	return nil
}

// parseFCTL parses an fcTL chunk (expects exactly 26 bytes).
//
// State transitions:
// - stateHaveIHDR     -> nothing to commit -> stateInFrame
// - stateDefaultImage -> commitFrame ( -> Background)  -> stateInFrame
// - stateInFrame      -> commitFrame (previous frame) -> stateInFrame
func (d *decoder) parseFCTL(data []byte) error {
	if len(data) != 26 {
		return fmt.Errorf("%w: fcTL length must be 26, got %d", ErrInvalidChunk, len(data))
	}
	if err := d.commitFrame(); err != nil {
		return err
	}
	d.curRawFrame = &rawFrame{
		seqNum:   binary.BigEndian.Uint32(data[0:4]),
		width:    binary.BigEndian.Uint32(data[4:8]),
		height:   binary.BigEndian.Uint32(data[8:12]),
		xOffset:  binary.BigEndian.Uint32(data[12:16]),
		yOffset:  binary.BigEndian.Uint32(data[16:20]),
		delayNum: binary.BigEndian.Uint16(data[20:22]),
		delayDen: binary.BigEndian.Uint16(data[22:24]),
		dispOp:   data[24],
		blendOp:  data[25],
	}
	d.state = stateInFrame
	return nil
}

// parseIDAT handles an IDAT chunk, accumulating its payload into d.idatBuffs.
//
// State Transitions:
// - stateHaveIHDR     -> stateDefaultImage (first IDAT before any fcTL)
// - stateDefaultImage -> stateDefaultImage (additional default-image IDAT)
// - stateInFrame      -> stateInFrame      (frame-0 IDAT data)
func (d *decoder) parseIDAT(data []byte) error {
	switch d.state {
	case stateHaveIHDR:
		// First IDAT before any fcTL; this is the default (background) image.
		d.state = stateDefaultImage
		fallthrough
	case stateDefaultImage, stateInFrame:
		d.idatBuffs = append(d.idatBuffs, data)
	default:
		// IDAT after IEND or other unexpected state: ignore.
	}
	return nil
}

// parseFDAT handles an fdAT chunk.
// Strips the 4-byte sequence number and accumulates the remaining payload.
// Only valid in stateInFrame; orphan fdAAT chunks (no preceding fcTL) are ignored.
func (d *decoder) parseFDAT(data []byte) error {
	if d.state != stateInFrame {
		return nil
	}
	if len(data) < 4 {
		return fmt.Errorf("%w: fdAT length must be at least 4, got %d", ErrInvalidChunk, len(data))
	}
	// data[0:4] is the sequence number; data[4:] is the raw image data.
	d.idatBuffs = append(d.idatBuffs, data[4:])
	return nil
}

// parseIEND handles the IEND chunk by commiting the last pending frame.
func (d *decoder) parseIEND() error {
	if err := d.commitFrame(); err != nil {
		return err
	}
	d.state = stateDone
	return nil
}

// commitFrame decodes the accumulated IDAT payloads and stores the result:
//   - curRawFrame == nil (default image): decoded image -> d.apng.Background
//   - curRawFrame != nil (animation frame): decoded image + metadata -> d.apng.Frames
//
// No-op when idatBuffs is empty (e.g. back-to-back fcTL without pixel data).
func (d *decoder) commitFrame() error {
	if len(d.idatBuffs) == 0 {
		d.curRawFrame = nil
		return nil
	}

	img, err := d.decodeImageFromIDAT()
	if err != nil {
		return fmt.Errorf("apng: decode frame image: %w", err)
	}

	if d.curRawFrame == nil {
		// Default image: stored on APNG.Background, not as an animation frame.
		if d.apng != nil {
			d.apng.Background = img
		}
	} else {
		rf := d.curRawFrame
		frame := Frame{
			Image:     img,
			XOffset:   int(rf.xOffset),
			YOffset:   int(rf.yOffset),
			DelayNum:  rf.delayNum,
			DelayDen:  rf.delayDen,
			DisposeOp: DisposeOp(rf.dispOp),
			BlendOp:   BlendOp(rf.blendOp),
		}
		if d.apng != nil {
			d.apng.Frames = append(d.apng.Frames, frame)
		}
	}

	d.curRawFrame = nil
	d.idatBuffs = nil
	return nil
}

// decodeImageFromIDAT reassembles a minimal valid PNG in memory
// (signature + IHDR + single IDAT + IEND) and decodes it with the standard library.
//
// The IHDR width/height comes from curRawFrame when available (frame fragment dimensions),
// falling back to d.ihdr (canvas dimensions, used for the default image).
// Color-space fields (bitDepth, colorType, etc.) always come from d.ihdr.
func (d *decoder) decodeImageFromIDAT() (image.Image, error) {
	var buf bytes.Buffer
	buf.Write(pngHeader)

	width := d.ihdr.width
	height := d.ihdr.height
	if d.curRawFrame != nil {
		width = d.curRawFrame.width
		height = d.curRawFrame.height
	}

	ihdrData := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdrData[0:4], width)
	binary.BigEndian.PutUint32(ihdrData[4:8], height)
	ihdrData[8] = d.ihdr.bitDepth
	ihdrData[9] = d.ihdr.colorType
	ihdrData[10] = d.ihdr.compress
	ihdrData[11] = d.ihdr.filter
	ihdrData[12] = d.ihdr.interlace
	if err := writeChunk(&buf, "IHDR", ihdrData); err != nil {
		return nil, fmt.Errorf("write IHDR: %w", err)
	}

	// Merge all accumulated IDAT payloads into a single chunk.
	var merged []byte
	for _, b := range d.idatBuffs {
		merged = append(merged, b...)
	}
	if err := writeChunk(&buf, "IDAT", merged); err != nil {
		return nil, fmt.Errorf("write IDAT: %w", err)
	}

	if err := writeChunk(&buf, "IEND", nil); err != nil {
		return nil, fmt.Errorf("write IEND: %w", err)
	}

	return png.Decode(&buf)
}
