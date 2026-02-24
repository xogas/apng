package apng

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
)

// encoder encodes an APNG to a PNG stream.
type encoder struct {
	w    io.Writer
	apng *APNG
}

// framePlan holds the all derived data needed to emit on APNG frame.
type framePlan struct {
	frame            Frame
	idatPayloads     [][]byte
	width, height    uint32
	xOffset, yOffset uint32
	nextPrevCanvas   *image.RGBA
}

// seqCursor maintains the monotonically increasing sequence number for fcTL/fdAT chunks.
type seqCursor struct{ next uint32 }

func (c *seqCursor) nextSeq() uint32 {
	seq := c.next
	c.next++
	return seq
}

func newEncoder(apng *APNG, w io.Writer) (*encoder, error) {
	if _, err := w.Write(pngHeader); err != nil {
		return nil, fmt.Errorf("apng: write PNG signature: %w", err)
	}
	return &encoder{apng: apng, w: w}, nil
}

// encode is the main encoding flow.
func (e *encoder) encode() error {
	// 1. calidate inputs
	if len(e.apng.Frames) == 0 {
		return errors.New("apng: no frames to encode")
	}
	for i, f := range e.apng.Frames {
		if f.Image == nil {
			return fmt.Errorf("apng: frame %d has nil Image", i)
		}
		if f.XOffset < 0 || f.YOffset < 0 {
			return fmt.Errorf("apng: frame %d has negative offset (%d, %d)", i, f.XOffset, f.YOffset)
		}
	}

	// 2. canvas size
	canvasW, canvasH := e.canvasSize()
	if canvasW == 0 || canvasH == 0 {
		return fmt.Errorf("apng: canvas size is zero")
	}

	// 3. extract color parameters from first frame
	refIHDR, _, err := e.encodeImageToIDAT(e.apng.Frames[0].Image)
	if err != nil {
		return fmt.Errorf("apng: encode reference frame: %w", err)
	}

	// 4. write IHDR + acTL
	if err := e.writeHeaderChunks(refIHDR, canvasW, canvasH, uint32(len(e.apng.Frames))); err != nil {
		return fmt.Errorf("apng: write header chunks: %w", err)
	}

	// 5. optional: write Background as default image IDAT
	if e.apng.Background != nil {
		_, bgPayloads, err := e.encodeImageToIDAT(e.apng.Background)
		if err != nil {
			return fmt.Errorf("apng: encode background image: %w", err)
		}
		for _, payload := range bgPayloads {
			if err := writeChunk(e.w, chunkIDAT, payload); err != nil {
				return fmt.Errorf("apng: write background IDAT: %w", err)
			}
		}
	}

	// 6. Encode animation frames
	var preCanvas *image.RGBA
	seq := &seqCursor{}

	for i, frame := range e.apng.Frames {
		plan, err := e.planFrame(frame, preCanvas, canvasW, canvasH)
		if err != nil {
			return fmt.Errorf("apng: plan frame %d: %w", i, err)
		}

		if err := e.writeFCTL(plan, seq.nextSeq()); err != nil {
			return fmt.Errorf("apng: write fcTL for frame %d: %w", i, err)
		}

		if err := e.writeFramePayload(plan.idatPayloads, seq, i == 0); err != nil {
			return fmt.Errorf("apng: write frame payload for frame %d: %w", i, err)
		}

		preCanvas = plan.nextPrevCanvas
	}

	// 7. IEND
	if err := writeChunk(e.w, chunkIEND, nil); err != nil {
		return fmt.Errorf("apng: write IEND: %w", err)
	}

	return nil
}

// canvasSize returns the canvas width and height.
// Uses APNG.Width/Height directly when non-zero; otherwise computes from frame bounds.
func (e *encoder) canvasSize() (w, h int) {
	if e.apng.Width > 0 && e.apng.Height > 0 {
		return int(e.apng.Width), int(e.apng.Height)
	}

	for _, f := range e.apng.Frames {
		if f.Image == nil {
			continue
		}
		right := f.XOffset + f.Image.Bounds().Dx()
		bottom := f.YOffset + f.Image.Bounds().Dy()
		if right > w {
			w = right
		}
		if bottom > h {
			h = bottom
		}
	}
	return
}

// writeHeaderChunks writes the IHDR and acTL chunks.
// The color-space fields of IHDR (bitDepth, colorType, etc.) are taken from refIHDR;
// width and height are replaced with canvas dimensions.
func (e *encoder) writeHeaderChunks(refIHDR []byte, canvasW, canvasH int, frameCount uint32) error {
	canvasIHDR := rebuildIHDR(refIHDR, canvasW, canvasH)
	if err := writeChunk(e.w, chunkIHDR, canvasIHDR); err != nil {
		return fmt.Errorf("apng: write IHDR: %w", err)
	}

	acTL := make([]byte, 8)
	binary.BigEndian.PutUint32(acTL[0:4], frameCount)
	binary.BigEndian.PutUint32(acTL[4:8], e.apng.LoopCount)
	if err := writeChunk(e.w, chunkACTL, acTL); err != nil {
		return fmt.Errorf("apng: write acTL: %w", err)
	}
	return nil
}

// planFrame computes the diff-cropped sub-image for frame and encodes it IDAT payloads.
//
// Steps:
//  1. compositeOnto: blend frame.Image onto a clone of prevCanvas -> currCanvas
//  2. diffRegion: find the bounding box of changed pixels betwen prevCanvas and currCanvas
//  3. Convert canvas-coordinates region to frame-local coordinates and crop frame.Image
//  4. encodeImageToIDAT: encode te cropped sub-image
//  5. deriveNextPrevCanvas: apply DisposeOp to determine the next prevCanvas
func (e *encoder) planFrame(frame Frame, prevCanvas *image.RGBA, canvasW, canvasH int) (*framePlan, error) {
	currCanvas := compositeOnto(prevCanvas, canvasW, canvasH, frame)

	canvasRegion := diffRegion(prevCanvas, currCanvas)

	// Convert from canvas coordinates to frame-local coordinates.
	localRegion := image.Rect(
		canvasRegion.Min.X-frame.XOffset,
		canvasRegion.Min.Y-frame.YOffset,
		canvasRegion.Max.X-frame.XOffset,
		canvasRegion.Max.Y-frame.YOffset,
	).Intersect(toRGBA(frame.Image).Bounds())

	if localRegion.Empty() {
		// keep at least 1*1 to satisfy the APNG spec (zero-size frames are invalid)
		localRegion = image.Rect(0, 0, 1, 1)
	}

	subImage := cropImage(toRGBA(frame.Image), localRegion)

	_, idatPayloads, err := e.encodeImageToIDAT(subImage)
	if err != nil {
		return nil, fmt.Errorf("encodeImageToIDAT: %w", err)
	}

	return &framePlan{
		frame:          frame,
		idatPayloads:   idatPayloads,
		width:          uint32(localRegion.Dx()),
		height:         uint32(localRegion.Dy()),
		xOffset:        uint32(int(frame.XOffset) + localRegion.Min.X),
		yOffset:        uint32(int(frame.YOffset) + localRegion.Min.Y),
		nextPrevCanvas: deriveNextPrevCanvas(frame, prevCanvas, currCanvas),
	}, nil
}

// writeFCTL write one fcTL chunk consuming one sequence number.
func (e *encoder) writeFCTL(plan *framePlan, seq uint32) error {
	payload := make([]byte, 26)
	binary.BigEndian.PutUint32(payload[0:4], seq)
	binary.BigEndian.PutUint32(payload[4:8], plan.width)
	binary.BigEndian.PutUint32(payload[8:12], plan.height)
	binary.BigEndian.PutUint32(payload[12:16], plan.xOffset)
	binary.BigEndian.PutUint32(payload[16:20], plan.yOffset)
	binary.BigEndian.PutUint16(payload[20:22], plan.frame.DelayNum)
	binary.BigEndian.PutUint16(payload[22:24], plan.frame.DelayDen)
	payload[24] = uint8(plan.frame.DisposeOp)
	payload[25] = uint8(plan.frame.BlendOp)

	if err := writeChunk(e.w, chunkFCTL, payload); err != nil {
		return fmt.Errorf("apng: write fcTL: %w", err)
	}
	return nil
}

// writeFramePayload  writes pixel data for one frame:
//   - firstFrame=true:  IDAT chunks (sequence number is not consumed)
//   - firstFrame=false: fdAT chunks (each consumes one sequence number)
func (e *encoder) writeFramePayload(idatPayloads [][]byte, seq *seqCursor, firstFrame bool) error {
	for _, payload := range idatPayloads {
		if firstFrame {
			if err := writeChunk(e.w, chunkIDAT, payload); err != nil {
				return fmt.Errorf("apng: write IDAT: %w", err)
			}
			continue
		}

		buf := make([]byte, 4+len(payload))
		binary.BigEndian.PutUint32(buf[0:4], seq.nextSeq())
		copy(buf[4:], payload)
		if err := writeChunk(e.w, chunkFDAT, buf); err != nil {
			return fmt.Errorf("apng: write fdAT: %w", err)
		}
	}
	return nil
}

// encodeImageToIDAT encodes img using the standard png encoder, then extracts
// the IHDR payload and all IDAT payloads from the resulting PNG stream.
func (e *encoder) encodeImageToIDAT(img image.Image) (ihdr []byte, idatPayloads [][]byte, err error) {
	var buf bytes.Buffer
	if err = png.Encode(&buf, img); err != nil {
		return nil, nil, err
	}
	// Skip the 8-byte PNG signature.
	if _, err = io.CopyN(io.Discard, &buf, int64(len(pngHeader))); err != nil {
		return nil, nil, err
	}

	return extractIHDRAndIDAT(&buf)
}

// extractIHDRAndIDAT reads a PNG chunk stream (staring after the signature) and
// returns the IHDR payload and all IDAT payloads.
func extractIHDRAndIDAT(r io.Reader) (ihdr []byte, idatPayloads [][]byte, err error) {
	for {
		typ, data, readErr := readChunk(r)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, nil, readErr
		}

		switch typ {
		case chunkIHDR:
			ihdr = append([]byte(nil), data...)
		case chunkIDAT:
			idatPayloads = append(idatPayloads, append([]byte(nil), data...))
		case chunkIEND:
			if ihdr == nil {
				return nil, nil, fmt.Errorf("%w: missing IHDR in encoded PNG", ErrInvalidChunk)
			}
			if len(idatPayloads) == 0 {
				return nil, nil, fmt.Errorf("%w: missing IDAT in encoded PNG", ErrInvalidChunk)
			}
			return ihdr, idatPayloads, nil
		}
	}

	if ihdr == nil {
		return nil, nil, fmt.Errorf("%w: missing IHDR in encoded PNG", ErrInvalidChunk)
	}
	if len(idatPayloads) == 0 {
		return nil, nil, fmt.Errorf("%w: missing IDAT in encoded PNG", ErrInvalidChunk)
	}
	return ihdr, idatPayloads, nil
}

// CompositeOnto blends frame.Image onto a clone of prev according to frame.BlendOp.
// When prev is nil, a blank canvas of canvasW*canvasH is created.
func compositeOnto(prev *image.RGBA, canvasW, canvasH int, frame Frame) *image.RGBA {
	var dst *image.RGBA
	if prev != nil {
		dst = cloneRGBA(prev)
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, canvasW, canvasH))
	}
	if frame.Image == nil {
		return dst
	}

	region := frame.Bounds()
	switch frame.BlendOp {
	case BlendOpOver:
		draw.Draw(dst, region, frame.Image, frame.Image.Bounds().Min, draw.Over)
	default: // BlendOpSource
		draw.Draw(dst, region, frame.Image, frame.Image.Bounds().Min, draw.Src)
	}
	return dst
}

// diffRegion returns the bounding box of pixels that differ betwen prev and curr.
// Returns curr.Bounds() when prev is nil (first frame: entire canvas is "changed").
// Returns a 1*1 region when no pixels differ (APNG forbids zero-size frames).
//
// Implementation uses bytes.Equal for row-level comparison against the underlying
// Pix slice, which is 1-2 orders of magnitude faster than per-pixel RGBAAt calls
// and allows the compiler/CPU to apply SIMD vectorization.
func diffRegion(prev, curr *image.RGBA) image.Rectangle {
	bounds := curr.Bounds()
	if prev == nil {
		return bounds
	}

	stride := curr.Stride
	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X, bounds.Min.Y

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		rowStart := (y - bounds.Min.Y) * stride
		rowEnd := rowStart + bounds.Dx()*4
		currRow := curr.Pix[rowStart:rowEnd]
		prevRow := prev.Pix[rowStart:rowEnd]

		if bytes.Equal(currRow, prevRow) {
			continue
		}

		if y < minY {
			minY = y
		}
		if y+1 > maxY {
			maxY = y + 1
		}

		// Scan this row for changed pixel columns.
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			off := (x - bounds.Min.X) * 4
			if currRow[off] != prevRow[off] ||
				currRow[off+1] != prevRow[off+1] ||
				currRow[off+2] != prevRow[off+2] ||
				currRow[off+3] != prevRow[off+3] {
				if x < minX {
					minX = x
				}
				if x+1 > maxX {
					maxX = x + 1
				}
			}
		}
	}

	if minX >= maxX || minY >= maxY {
		return image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Min.X+1, bounds.Min.Y+1)
	}

	return image.Rect(minX, minY, maxX, maxY)
}

// deriveNextPrevCanvas determines the prevCanvas state after this frame is displayed.
// applying DisposeOp per the APNG specification.
func deriveNextPrevCanvas(frame Frame, prevCanvas, currCanvas *image.RGBA) *image.RGBA {
	switch frame.DisposeOp {
	case DisposeOpBackground:
		// Clear the frame region to fully transparent after display.
		cloned := cloneRGBA(currCanvas)
		clearRegionRGBA(cloned, frame.Bounds())
		return cloned
	case DisposeOpPrevious:
		// Restore the canvas to the state before this frame was rendered.
		return prevCanvas
	default: // DisposeOpNone
		return currCanvas
	}
}

// rebuildIHDR returns a copy of ref with the width and height fields replaced.
func rebuildIHDR(ref []byte, width, height int) []byte {
	ihdr := make([]byte, len(ref))
	copy(ihdr, ref)
	if len(ihdr) >= 8 {
		binary.BigEndian.PutUint32(ihdr[0:4], uint32(width))
		binary.BigEndian.PutUint32(ihdr[4:8], uint32(height))
	}
	return ihdr
}

// cropImage returns a new *image.RGBA containing the region r of src, with Min at (0,0).
func cropImage(src *image.RGBA, r image.Rectangle) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(dst, dst.Bounds(), src, r.Min, draw.Src)
	return dst
}

// toRGBA converts any image.Image to *image.RGBA with Min at (0,0).
// Returns src unchanged if it is already the right type and origin.
func toRGBA(src image.Image) *image.RGBA {
	if rgba, ok := src.(*image.RGBA); ok && rgba.Bounds().Min == (image.Point{}) {
		return rgba
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// cloneRGBA returns a deep copy of src.
func cloneRGBA(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}

// clearRegionRGBA sets all pixels in r to fully transparent black.
func clearRegionRGBA(img *image.RGBA, r image.Rectangle) {
	draw.Draw(img, r, image.Transparent, image.Point{}, draw.Src)
}
