package app

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

var errUnsupportedContentEncoding = errors.New("unsupported content encoding")

// zstdContentEncodingMaxWindow 取 RFC 9659 对 HTTP zstd 内容编码的窗口上限 8MB；
// klauspost 默认允许 512MB 窗口并开多个解码 goroutine，入站请求体由调用方控制，
// 不能让一个帧头声明的窗口撑大内存。
const zstdContentEncodingMaxWindow = 8 << 20

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
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
	decoded, err := decodeContentEncodingBody(resp.Body, contentEncoding)
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

// decodeContentEncodingBody 按 Content-Encoding 逆序叠加解码器；空值与 identity
// 原样返回 body。失败时 body 已关闭。
func decodeContentEncodingBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("content-encoded body is nil")
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
				return nil, fmt.Errorf("decode gzip body: %w", err)
			}
			reader = decoder
			decoderClosers = append(decoderClosers, decoder.Close)
		case "deflate":
			decoder, err := newDeflateReader(reader)
			if err != nil {
				cleanup()
				return nil, err
			}
			reader = decoder
			decoderClosers = append(decoderClosers, decoder.Close)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoder, err := zstd.NewReader(reader,
				zstd.WithDecoderConcurrency(1),
				zstd.WithDecoderMaxWindow(zstdContentEncodingMaxWindow))
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("decode zstd body: %w", err)
			}
			reader = decoder
			decoderClosers = append(decoderClosers, func() error {
				decoder.Close()
				return nil
			})
		default:
			cleanup()
			return nil, fmt.Errorf("%w %q", errUnsupportedContentEncoding, encoding)
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
	return &compositeReadCloser{Reader: reader, closers: closers}, nil
}

func newDeflateReader(reader io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(reader)
	header, err := buffered.Peek(2)
	if err == nil && isZlibHeader(header) {
		decoder, decodeErr := zlib.NewReader(buffered)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode zlib body: %w", decodeErr)
		}
		return decoder, nil
	}
	return flate.NewReader(buffered), nil
}

func isZlibHeader(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf, flg := header[0], header[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}
