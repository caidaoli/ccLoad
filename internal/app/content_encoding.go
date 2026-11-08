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
	"slices"
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
	encodings, _, err := parseContentEncodings(contentEncoding)
	if err != nil {
		_ = resp.Body.Close()
		return err
	}
	decoded, err := decodeContentEncodingBody(resp.Body, encodings)
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

/**
 * @brief 校验内容编码，并为每层解码器计算有界工作区预留。
 * @param contentEncoding 网络请求或响应中的 Content-Encoding 字段。
 * @return 按声明顺序排列的规范编码，不包含 identity。
 * @return 解码期间预留的工作区字节数，包含窗口及块缓冲余量。
 * @return 不支持的编码返回 errUnsupportedContentEncoding，否则返回 nil。
 */
func parseContentEncodings(contentEncoding string) ([]string, int64, error) {
	var encodings []string
	var workspace int64
	for encoding := range strings.SplitSeq(contentEncoding, ",") {
		encoding = strings.ToLower(strings.TrimSpace(encoding))
		switch encoding {
		case "", "identity":
			continue
		case "gzip", "deflate":
			workspace += 256 << 10
		case "br":
			// Brotli 的 24-bit 窗口之外，还需保留块解码缓冲。
			workspace += 32 << 20
		case "zstd":
			// 拼接帧切换窗口时可能同时持有新旧 ring，另需块与序列表空间。
			workspace += 4 * zstdContentEncodingMaxWindow
		default:
			return nil, 0, fmt.Errorf("%w %q", errUnsupportedContentEncoding, encoding)
		}
		encodings = append(encodings, encoding)
	}
	return encodings, workspace, nil
}

/**
 * @brief 按已校验的 Content-Encoding 逆序叠加解码器。
 * @param body 原始网络正文。
 * @param encodings parseContentEncodings 返回的规范编码。
 * @return 解码后的正文，调用方须关闭；无编码时原样返回 body。
 * @return 构造解码器失败时返回错误，并关闭已构造的解码器与 body。
 */
func decodeContentEncodingBody(body io.ReadCloser, encodings []string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("content-encoded body is nil")
	}
	reader := io.Reader(body)
	decoderClosers := make([]func() error, 0, len(encodings))
	cleanup := func() {
		for _, decoderCloser := range slices.Backward(decoderClosers) {
			_ = decoderCloser()
		}
		_ = body.Close()
	}
	for _, encoding := range slices.Backward(encodings) {
		switch encoding {
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
		}
	}
	if len(decoderClosers) == 0 && reader == body {
		return body, nil
	}
	closers := make([]func() error, 0, len(decoderClosers)+1)
	for _, decoderCloser := range slices.Backward(decoderClosers) {
		closers = append(closers, decoderCloser)
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
