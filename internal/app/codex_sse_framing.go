package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"ccLoad/internal/protocol"
)

// codexSSEFramingReader repairs the narrow malformed framing emitted by some
// Codex Responses upstreams. It only inserts a blank line when the next
// response event proves the previous event is complete; EOF never dispatches
// an unterminated event.
type codexSSEFramingReader struct {
	src          io.Reader
	closer       io.Closer
	closeOnce    sync.Once
	closeErr     error
	readBuf      []byte
	readOff      int
	readLen      int
	out          []byte
	sourceErr    error
	fatalErr     error
	done         bool
	state        codexSSEBlockState
	blockBytes   int
	linePrefix   []byte
	lineLast     byte
	candidate    []byte
	candidateOn  bool
	lineChecked  bool
	streamPrefix bool
}

type codexSSEBlockState struct {
	events   int
	datas    int
	lines    int
	ordered  bool
	response bool
	eolCRLF  bool
}

func newCodexSSEFramingReader(src io.Reader) *codexSSEFramingReader {
	if existing, ok := src.(*codexSSEFramingReader); ok {
		return existing
	}
	r := &codexSSEFramingReader{
		src:          src,
		readBuf:      make([]byte, SSEBufferSize),
		linePrefix:   make([]byte, 0, codexSSEPrefixLimit),
		state:        codexSSEBlockState{ordered: true},
		streamPrefix: true,
	}
	if c, ok := src.(io.Closer); ok {
		r.closer = c
	}
	return r
}

// wrapCodexSSEBody preserves the upstream body's Close contract while applying
// the bounded framing filter exactly once.
func wrapCodexSSEBody(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return nil
	}
	if _, ok := body.(*codexSSEFramingReader); ok {
		return body
	}
	return newCodexSSEFramingReader(body)
}

// wrapCodexSSEResponseBody applies the framing fix at an SSE consumer boundary.
// The protocol guard keeps non-Codex streams byte-for-byte untouched.
func wrapCodexSSEResponseBody(resp *http.Response, upstreamProtocol protocol.Protocol, isSSE bool) {
	if !isSSE || upstreamProtocol != protocol.Codex || resp == nil || resp.Body == nil {
		return
	}
	resp.Body = wrapCodexSSEBody(resp.Body)
}

func (r *codexSSEFramingReader) Close() error {
	r.closeOnce.Do(func() {
		if r.closer != nil {
			r.closeErr = r.closer.Close()
		}
	})
	return r.closeErr
}

func (r *codexSSEFramingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.out) == 0 && !r.done {
		if r.readOff < r.readLen {
			r.process()
			continue
		}
		if r.sourceErr != nil {
			r.finishSource()
			continue
		}
		n, err := r.src.Read(r.readBuf)
		if n > 0 {
			r.readOff = 0
			r.readLen = n
			if err != nil {
				r.sourceErr = err
			}
			continue
		}
		if err == nil {
			err = io.EOF
		}
		r.sourceErr = err
	}
	if len(r.out) > 0 {
		n := copy(p, r.out)
		r.out = r.out[n:]
		return n, nil
	}
	if r.fatalErr != nil {
		err := r.fatalErr
		r.fatalErr = nil
		return 0, err
	}
	if r.sourceErr != nil {
		err := r.sourceErr
		r.sourceErr = nil
		return 0, err
	}
	return 0, io.EOF
}

const (
	codexSSEPrefixLimit = 32
)

func (r *codexSSEFramingReader) process() {
	for r.readOff < r.readLen {
		if r.candidateOn {
			b := r.readBuf[r.readOff]
			r.candidate = append(r.candidate, b)
			r.readOff++
			if b == '\n' {
				r.candidateOn = false
				r.lineChecked = true
				r.emitHeldCandidate()
				if r.fatalErr != nil {
					return
				}
				continue
			}
			matched, mismatch := codexSSETypedEventPrefixStatus(r.candidate)
			if matched {
				r.candidateOn = false
				r.lineChecked = true
				if r.state.eolCRLF {
					r.out = append(r.out, '\r', '\n')
				} else {
					r.out = append(r.out, '\n')
				}
				sseFramingRepairs.Add(1)
				r.state.reset()
				r.blockBytes = 0
				r.emitHeldCandidate()
				if r.fatalErr != nil {
					return
				}
				continue
			}
			if mismatch || len(r.candidate) >= codexSSEPrefixLimit {
				r.candidateOn = false
				r.lineChecked = true
				r.emitHeldCandidate()
				if r.fatalErr != nil {
					return
				}
			}
			continue
		}
		if !r.lineChecked && len(r.linePrefix) == 0 && r.state.repairable() {
			r.candidateOn = true
			r.candidate = r.candidate[:0]
			continue
		}
		r.lineChecked = true
		rest := r.readBuf[r.readOff:r.readLen]
		n := bytes.IndexByte(rest, '\n')
		if n < 0 {
			n = len(rest)
		} else {
			n++
		}
		r.emitBytes(rest[:n])
		r.readOff += n
		if r.fatalErr != nil {
			return
		}
	}
}

func (r *codexSSEFramingReader) emitHeldCandidate() {
	r.emitBytes(r.candidate)
	r.candidate = r.candidate[:0]
}

func (r *codexSSEFramingReader) emitBytes(data []byte) {
	if len(data) == 0 {
		return
	}
	if r.blockBytes >= maxSSEEventBytes {
		r.fatalErr = fmt.Errorf("SSE event exceeds %d bytes", maxSSEEventBytes)
		r.done = true
		return
	}
	available := maxSSEEventBytes - r.blockBytes
	overflow := false
	if len(data) > available {
		data = data[:available]
		overflow = true
	}
	r.out = append(r.out, data...)
	r.blockBytes += len(data)
	content := data
	hasNL := data[len(data)-1] == '\n'
	if hasNL {
		content = data[:len(data)-1]
	}
	if len(content) > 0 {
		// Track the actual line tail before capping linePrefix. This also
		// handles a CR and its following LF arriving in separate reads.
		r.lineLast = content[len(content)-1]
		space := codexSSEPrefixLimit - len(r.linePrefix)
		if space > 0 {
			if len(content) > space {
				content = content[:space]
			}
			r.linePrefix = append(r.linePrefix, content...)
		}
	}
	if hasNL {
		r.finishLine()
	}
	if overflow {
		r.fatalErr = fmt.Errorf("SSE event exceeds %d bytes", maxSSEEventBytes)
		r.done = true
	}
}

func (r *codexSSEFramingReader) finishLine() {
	line := r.linePrefix
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	observedLine := line
	if r.streamPrefix {
		observedLine = normalizeSSEStreamPrefix(observedLine)
		if len(observedLine) > 0 && observedLine[0] != ':' {
			r.streamPrefix = false
		}
	}
	if len(line) == 0 {
		r.state.reset()
		r.blockBytes = 0
	} else if len(observedLine) > 0 {
		r.state.observe(observedLine, r.lineLast == '\r')
	}
	r.linePrefix = r.linePrefix[:0]
	r.lineLast = 0
	r.lineChecked = false
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// normalizeSSEStreamPrefix is shared by SSE detection and Codex framing.
// It only affects classification; callers continue emitting the original bytes.
func normalizeSSEStreamPrefix(prefix []byte) []byte {
	prefix = bytes.TrimPrefix(prefix, utf8BOM)
	return bytes.TrimLeft(prefix, " \t\r\n")
}

func (r *codexSSEFramingReader) finishSource() {
	if r.candidateOn {
		r.candidateOn = false
		r.lineChecked = true
		r.emitHeldCandidate()
	}
	r.done = true
}

func codexSSETypedEventPrefixStatus(prefix []byte) (matched, mismatch bool) {
	const eventPrefix = "event:"
	if len(prefix) > codexSSEPrefixLimit {
		return false, true
	}
	if len(prefix) <= len(eventPrefix) {
		return false, !bytes.HasPrefix([]byte(eventPrefix), prefix)
	}
	if !bytes.HasPrefix(prefix, []byte(eventPrefix)) {
		return false, true
	}
	value := bytes.TrimLeft(prefix[len(eventPrefix):], " \t")
	if len(value) == 0 {
		return false, false
	}
	want := []byte("response.")
	if len(value) < len(want) {
		return false, !bytes.HasPrefix(want, value)
	}
	return bytes.HasPrefix(value, want), !bytes.HasPrefix(value, want)
}

func (s *codexSSEBlockState) reset() {
	*s = codexSSEBlockState{ordered: true}
}

func (s *codexSSEBlockState) observe(line []byte, eolCRLF bool) {
	if len(line) == 0 {
		return
	}
	if bytes.HasPrefix(line, []byte(":")) {
		// Leading heartbeat comments do not belong to the event block. A
		// comment after an event/data line does, and therefore prevents repair.
		if s.lines > 0 {
			s.ordered = false
		}
		return
	} else if value, ok := bytes.CutPrefix(line, []byte("event:")); ok {
		s.lines++
		s.events++
		s.response = bytes.HasPrefix(bytes.TrimSpace(value), []byte("response."))
		s.ordered = s.ordered && s.lines == 1
	} else if _, ok := bytes.CutPrefix(line, []byte("data:")); ok {
		s.lines++
		s.datas++
		s.ordered = s.ordered && s.lines == 2
	} else {
		s.lines++
		s.ordered = false
	}
	s.eolCRLF = eolCRLF
}

func (s *codexSSEBlockState) repairable() bool {
	return s.ordered && s.lines == 2 && s.events == 1 && s.datas == 1 && s.response
}

var sseFramingRepairs atomic.Uint64
