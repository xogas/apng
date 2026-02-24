package apng

import (
	"image"
	"io"
	"time"
)

// DisposeOp specifies how the canvas should be updated after a frame is  displayed,
// before the next frame is rendered.
type DisposeOp uint8

const (
	// DisposeOpNone leaves the  canvas unchanged after this frame. (default)
	DisposeOpNone DisposeOp = iota
	// DisposeOpBackground clears the frame region to fully transparent black.
	DisposeOpBackground
	// DisposeOpPrevious restores the frame region to its state before this frame was rendered.
	DisposeOpPrevious
)

// BlendOp specifies whether the frame is composited onto the canvas or replaces it.
type BlendOp uint8

const (
	// BlendOpSource replaces the canvas region with the frame pixels (draw.Src).
	BlendOpSource BlendOp = iota
	// BlendOpOver composites the frame over the canvas using alpha blending (draw.Over). (default)
	BlendOpOver
)

// APNG represents an animated PNG image.
type APNG struct {
	// Width and Height are the canvas dimensions in pixels.
	//
	// When encoding, a zero value causes the encoder to compute the size
	// from the union of all frame bounds automatically.
	//
	// When decoding, these are populated from the IHDR chunk.
	// uint32 directly maps the IHDR binary field and rules out negative values at the type level.
	Width, Height uint32

	// Frames holds the animation frames in display order.
	// At least one frame is required for encoding.
	Frames []Frame

	// LoopCount specifies the number od times the animation loops.
	// 0 means loop forever.
	LoopCount uint32

	// Background is the optional static fallback image (the APNG "default image").
	// Non-APNG-aware viewers display this image; APNG viewers ignore it.
	//
	// Encoding: is non-nil, it is encode as IDAT written before the first fcTL.
	// Decoding: IDAT chunks appearing before the first fcTL are decoded into this field.
	Background image.Image
}

// CompositeFrames returns the fully composited canvas for each animation frame,
// in display order. Each element corresponds to a.Frames[i] and represents the
// complete visual state of the canvas after that frame is rendered.
//
// This applies BlendOp  and DisposeOp exactly as an APNG viewer would.
// The returned images are independent copies; modifying them does not affect a.
func (a *APNG) CompositeFrames() []*image.RGBA {
	canvasW, canvasH := int(a.Width), int(a.Height)
	// fallback: compute from frames if Width/Height are zero
	if canvasW == 0 || canvasH == 0 {
		for _, f := range a.Frames {
			if f.Image == nil {
				continue
			}
			if r := f.XOffset + f.Image.Bounds().Dx(); r > canvasW {
				canvasW = r
			}
			if b := f.YOffset + f.Image.Bounds().Dy(); b > canvasH {
				canvasH = b
			}
		}
	}

	result := make([]*image.RGBA, len(a.Frames))
	var prevCanvas *image.RGBA
	for i, frame := range a.Frames {
		canvas := compositeOnto(prevCanvas, canvasW, canvasH, frame)
		result[i] = cloneRGBA(canvas)
		prevCanvas = deriveNextPrevCanvas(frame, prevCanvas, canvas)
	}

	return result
}

// Frame represents one frame of an APNG animation.
//
// Image holds the raw pixel fragment for this frame, not the composited full canvas.
// When decoding, Image is the fragment decoded directly from IDAT/fdAT data.
// When encoding, the encoder composites it onto the canvas and performs diff cropping internally.
type Frame struct {
	// Image is the pixel content od this frame, with its own origin at (0,0).
	Image image.Image

	// XOffset and YOffset are offsets of the frame's top-left corner on the canvas.
	// Both must be >= 0 when encoding.
	XOffset, YOffset int

	// DelayNum and DelayDen are the numerator and denominator of the frame
	// display duration in seconds (duration = DelayNum / DelayDen).
	// If DelayDen is 0, it is treated as 100 per the APNG specification.
	DelayNum, DelayDen uint16

	// DisposeOp specifies how the canvas is updated after this frame is  displayed,
	DisposeOp DisposeOp

	// BlendOp specifies how the frame is composited onto the canvas.
	BlendOp BlendOp
}

// Bounds returns the rectangle this frame occupies on the canvas coordinate system.
// Return an empty rectangle if Image is nil.
func (f *Frame) Bounds() image.Rectangle {
	if f.Image == nil {
		return image.Rectangle{}
	}
	w := f.XOffset + f.Image.Bounds().Dx()
	h := f.YOffset + f.Image.Bounds().Dy()
	return image.Rect(f.XOffset, f.YOffset, w, h)
}

// Delay returns the display duration of this frame.
// If DelayDen is 0, it is treated as 100 per the APNG specification.
func (f *Frame) Delay() time.Duration {
	den := f.DelayDen
	if den == 0 {
		den = 100
	}
	return time.Duration(f.DelayNum) * time.Second / time.Duration(den)
}

// Decode reads an APNG from r.
//
// Reteurns ErrInvalidSignature if the stream does not begin with a PNG signature.
// Returns ErrNotAPNG if the stream is a valid PNG but contains no acTL chunk.
// CRC failures return a wrapped error containing ErrCRCMismatch,
// testable with errors.Is(err, apng.ErrCRCMismatch).
func Decode(r io.Reader) (*APNG, error) {
	decoder, err := newDecoder(r)
	if err != nil {
		return nil, err
	}

	return decoder.decode()
}

// Encode write a to w in APNG format.
//
// a.Width and a.Height may be zero; the encoder computes the canvas size
// from the bounding union of all frames in that case.
// All frames must have non-nil Image and non-negative XOffset and YOffset.
// The parameter order (w, a) matches the convention of png.Encode and json.NewEncoder.
func Encode(apng *APNG, w io.Writer) error {
	encoder, err := newEncoder(apng, w)
	if err != nil {
		return err
	}
	return encoder.encode()
}
