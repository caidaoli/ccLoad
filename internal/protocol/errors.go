package protocol

import "errors"

// ErrUnsupportedRequestShape marks structured inputs that the current protocol translators do not support yet.
var ErrUnsupportedRequestShape = errors.New("unsupported request shape for protocol transform")
