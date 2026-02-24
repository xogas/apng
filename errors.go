package apng

import "errors"

// Sentinel errors that callers can test with errors.Is.
var (
	// ErrInvalidSignature reports that the PNG signature is invalid.
	ErrInvalidSignature = errors.New("apng: invalid PNG signature")
	// ErrNotAPNG reports that the file is not an APNG file.
	ErrNotAPNG = errors.New("apng: not an APNG file")
	// ErrCRCMismatch reports that the CRC value of a chunk does not match the expected value.
	ErrCRCMismatch = errors.New("apng: CRC mismatch")
	// ErrInvalidChunk reports that a chunk is invalid (e.g., missing required fields, invalid data).
	ErrInvalidChunk = errors.New("apng: invalid chunk")
)
