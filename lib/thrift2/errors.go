package thrift2

import "errors"

var (
	ErrMaxBufferLen    = errors.New("thrift: max buffer len exceeded")
	ErrMaxMapElements  = errors.New("thrift: max map elements exceeded")
	ErrMaxSetElements  = errors.New("thrift: max set elements exceeded")
	ErrMaxListElements = errors.New("thrift: max list elements exceeded")
	ErrUnknownFunction = errors.New("thrift: unknown function")
	ErrMessageType     = errors.New("thrift: error message type")
	ErrFieldType       = errors.New("thrift: error field type")
	ErrVersion         = errors.New("thrift: unknown version")
	ErrSeqMismatch     = errors.New("thrift: seq mismatch")
	ErrDataLength      = errors.New("thrift: invalid data length")
	ErrDepthExceeded   = errors.New("thrift: depth limit exceeded")
)