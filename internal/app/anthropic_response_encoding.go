package app

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type anthropicCompositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *anthropicCompositeReadCloser) Close() error {
	var firstErr error
	for _, closeFn := range c.closers {
		if closeFn == nil {
			continue
		}
		if err := closeFn(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func decodeAnthropicResponse(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	contentEncoding := strings.Join(resp.Header.Values("Content-Encoding"), ",")
	if strings.TrimSpace(contentEncoding) == "" {
		return nil
	}
	decoded, err := decodeAnthropicResponseBody(resp.Body, contentEncoding)
	if err != nil {
		return err
	}
	resp.Body = decoded
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return nil
}

func decodeAnthropicResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("anthropic response body is nil")
	}
	encodings := strings.Split(contentEncoding, ",")
	reader := io.Reader(body)
	decoderClosers := make([]func() error, 0, len(encodings))
	cleanup := func() {
		for i := len(decoderClosers) - 1; i >= 0; i-- {
			_ = decoderClosers[i]()
		}
		_ = body.Close()
	}
	for index := len(encodings) - 1; index >= 0; index-- {
		switch encoding := strings.TrimSpace(strings.ToLower(encodings[index])); encoding {
		case "", "identity":
			continue
		case "gzip":
			decoder, err := gzip.NewReader(reader)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("decode Anthropic gzip response: %w", err)
			}
			reader = decoder
			decoderClosers = append(decoderClosers, decoder.Close)
		case "deflate":
			decoder, err := newAnthropicDeflateReader(reader)
			if err != nil {
				cleanup()
				return nil, err
			}
			reader = decoder
			decoderClosers = append(decoderClosers, decoder.Close)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoder, err := zstd.NewReader(reader)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("decode Anthropic zstd response: %w", err)
			}
			reader = decoder
			decoderClosers = append(decoderClosers, func() error {
				decoder.Close()
				return nil
			})
		default:
			cleanup()
			return nil, fmt.Errorf("unsupported Anthropic content encoding %q", encoding)
		}
	}
	if len(decoderClosers) == 0 && reader == body {
		return body, nil
	}
	closers := make([]func() error, 0, len(decoderClosers)+1)
	for index := len(decoderClosers) - 1; index >= 0; index-- {
		closers = append(closers, decoderClosers[index])
	}
	closers = append(closers, body.Close)
	return &anthropicCompositeReadCloser{Reader: reader, closers: closers}, nil
}

func newAnthropicDeflateReader(reader io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(reader)
	header, err := buffered.Peek(2)
	if err == nil && isAnthropicZlibHeader(header) {
		decoder, decodeErr := zlib.NewReader(buffered)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode Anthropic zlib response: %w", decodeErr)
		}
		return decoder, nil
	}
	return flate.NewReader(buffered), nil
}

func isAnthropicZlibHeader(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf, flg := header[0], header[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}
