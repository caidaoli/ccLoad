package qoderumid

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"debug/elf"
	"debug/macho"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	metadataURL = "https://registry.npmjs.org/@qoder-ai%2fqodercli/latest"
	maxMetadata = 1 << 20
	maxArchive  = 128 << 20
	maxExpanded = 512 << 20
	maxBundle   = 256 << 20
	maxBinary   = 64 << 20
	bundleEntry = "package/bundle/qodercli.js"
)

func (m *Manager) install(parent context.Context) error {
	if !m.supported() {
		return fmt.Errorf("automatic Qoder UMID installation is unsupported on %s/%s; install the official Qoder client or qodercli", m.goos, m.arch)
	}
	if m.dataDir == "" {
		return errors.New("qoder UMID installation requires a persistent data directory")
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	metadata, err := m.download(ctx, metadataURL, maxMetadata)
	if err != nil {
		return fmt.Errorf("qoder UMID metadata: %w", err)
	}
	var pkg struct {
		Version string `json:"version"`
		Dist    struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	if json.Unmarshal(metadata, &pkg) != nil || pkg.Version == "" {
		return errors.New("invalid official qodercli package metadata")
	}
	expected, err := integrityDigest(pkg.Dist.Integrity)
	if err != nil {
		return err
	}
	if !officialURL(pkg.Dist.Tarball) {
		return errors.New("qodercli tarball must use the official npm registry")
	}
	archive, err := m.download(ctx, pkg.Dist.Tarball, maxArchive)
	if err != nil {
		return fmt.Errorf("qoder UMID package: %w", err)
	}
	digest := sha512.Sum512(archive)
	if subtle.ConstantTimeCompare(digest[:], expected) != 1 {
		return errors.New("qodercli package SHA-512 integrity mismatch")
	}
	bundle, err := readBundle(archive)
	if err != nil {
		return err
	}
	payload, err := extractBinary(bundle, m.goos, m.arch)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(m.installedPath())
	if os.MkdirAll(dir, 0700) != nil {
		return errors.New("cannot create Qoder UMID data directory")
	}
	file, err := os.CreateTemp(dir, ".runtime-info-*")
	if err != nil {
		return errors.New("cannot stage Qoder UMID component")
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(payload); err != nil {
		_ = file.Close()
		return errors.New("cannot write Qoder UMID component")
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("cannot sync Qoder UMID component")
	}
	if err = file.Close(); err != nil {
		return errors.New("cannot close Qoder UMID component")
	}
	// Validate the staged file as well, before granting execute permission or publishing it.
	staged, err := os.Open(file.Name())
	if err != nil {
		return errors.New("cannot reopen staged Qoder UMID component")
	}
	err = validateExecutable(staged, m.goos, m.arch)
	closeErr := staged.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return errors.New("cannot close staged Qoder UMID component")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if os.Chmod(file.Name(), 0700) != nil {
		return errors.New("cannot set Qoder UMID component permissions")
	}
	if os.Rename(file.Name(), m.installedPath()) != nil {
		return errors.New("cannot publish Qoder UMID component")
	}
	return nil
}

func integrityDigest(integrity string) ([]byte, error) {
	if !strings.HasPrefix(integrity, "sha512-") {
		return nil, errors.New("official qodercli metadata requires SHA-512 integrity")
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(integrity, "sha512-"))
	if err != nil || len(digest) != sha512.Size {
		return nil, errors.New("invalid qodercli SHA-512 integrity")
	}
	return digest, nil
}

func officialURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "registry.npmjs.org" && u.User == nil && u.Fragment == "" && u.RawQuery == "" && (u.Path == "/@qoder-ai/qodercli/latest" || strings.HasPrefix(u.Path, "/@qoder-ai/qodercli/-/qodercli-"))
}

func (m *Manager) download(ctx context.Context, raw string, limit int64) ([]byte, error) {
	// The metadata URL uses an escaped slash; URL.Path is already unescaped here.
	if !officialURL(raw) {
		return nil, errors.New("only the official qodercli npm registry is allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, errors.New("invalid official qodercli URL")
	}
	client := *m.client
	originalRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !officialURL(req.URL.String()) {
			return errors.New("qodercli registry redirect rejected")
		}
		if originalRedirect != nil {
			return originalRedirect(req, via)
		}
		return nil
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("official qodercli registry request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official qodercli registry returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, errors.New("official qodercli response exceeds size limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, errors.New("cannot read official qodercli registry response")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("official qodercli response exceeds size limit")
	}
	return body, nil
}

func readBundle(archive []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, errors.New("invalid qodercli gzip archive")
	}
	defer func() { _ = gzipReader.Close() }()
	limited := &io.LimitedReader{R: gzipReader, N: maxExpanded + 1}
	reader := tar.NewReader(limited)
	var bundle []byte
	for entries := 0; ; entries++ {
		if entries > 10000 {
			return nil, errors.New("qodercli archive has too many entries")
		}
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, errors.New("invalid qodercli tar archive")
		}
		cleaned := path.Clean(header.Name)
		if path.IsAbs(header.Name) || strings.Contains(header.Name, "\\") || cleaned == ".." || strings.HasPrefix(cleaned, "../") || (cleaned != "package" && !strings.HasPrefix(cleaned, "package/")) {
			return nil, errors.New("unsafe qodercli archive path")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return nil, errors.New("unsupported qodercli archive entry")
		}
		if header.Name != bundleEntry {
			continue
		}
		if bundle != nil {
			return nil, errors.New("duplicate qodercli bundle entry")
		}
		if header.Size < 0 || header.Size > maxBundle {
			return nil, errors.New("qodercli bundle exceeds size limit")
		}
		bundle, err = io.ReadAll(io.LimitReader(reader, maxBundle+1))
		if err != nil || len(bundle) > maxBundle {
			return nil, errors.New("cannot read qodercli bundle")
		}
	}
	// Drain through the gzip checksum, also accounting for bytes after tar EOF.
	if _, err = io.Copy(io.Discard, limited); err != nil {
		return nil, errors.New("invalid qodercli gzip contents")
	}
	if limited.N <= 0 {
		return nil, errors.New("qodercli expanded archive exceeds size limit")
	}
	if bundle == nil {
		return nil, errors.New("official qodercli package has no supported bundle")
	}
	return bundle, nil
}

// extractBinary finds this platform's runtime-info among the base64 string literals
// qodercli embeds for every platform: ELF on Linux, Mach-O on macOS.
func extractBinary(bundle []byte, goos, arch string) ([]byte, error) {
	if arch != "amd64" && arch != "arm64" {
		return nil, errors.New("unsupported Qoder UMID architecture")
	}
	// Base64 of the ELF64 little-endian and Mach-O 64 magics.
	prefix := []byte("f0VMRgIB")
	if goos == "darwin" {
		prefix = []byte("z/rt/g")
	}
	for offset := 0; offset < len(bundle); {
		index := bytes.Index(bundle[offset:], prefix)
		if index < 0 {
			break
		}
		start := offset + index
		offset = start + len(prefix)
		if start == 0 || (bundle[start-1] != '"' && bundle[start-1] != '\'') {
			continue
		}
		quote := bundle[start-1]
		end := bytes.IndexByte(bundle[start:], quote)
		if end < 0 {
			break
		}
		offset = start + end + 1
		encoded := bundle[start : start+end]
		if len(encoded) > base64.StdEncoding.EncodedLen(maxBinary) {
			continue
		}
		// Check architecture before decoding a potentially large nonmatching payload.
		if len(encoded) < 88 {
			continue
		}
		head, err := base64.StdEncoding.Strict().DecodeString(string(encoded[:88]))
		if err != nil || !headMatches(head, goos, arch) {
			continue
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(string(encoded))
		if err != nil || len(decoded) > maxBinary {
			continue
		}
		if validateExecutable(bytes.NewReader(decoded), goos, arch) == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("official qodercli bundle has no valid UMID component for this platform")
}

func headMatches(head []byte, goos, arch string) bool {
	if goos == "darwin" {
		return len(head) >= 8 && macho.Cpu(binary.LittleEndian.Uint32(head[4:8])) == machoCPU(arch)
	}
	return len(head) >= 20 && elf.Machine(binary.LittleEndian.Uint16(head[18:20])) == elfMachine(arch)
}

func elfMachine(arch string) elf.Machine {
	if arch == "arm64" {
		return elf.EM_AARCH64
	}
	return elf.EM_X86_64
}

func machoCPU(arch string) macho.Cpu {
	if arch == "arm64" {
		return macho.CpuArm64
	}
	return macho.CpuAmd64
}

// validateExecutable parses the whole executable header for this platform.
func validateExecutable(r io.ReaderAt, goos, arch string) error {
	if arch != "amd64" && arch != "arm64" {
		return errors.New("unsupported Qoder UMID architecture")
	}
	if goos == "darwin" {
		file, err := macho.NewFile(r)
		if err != nil {
			return errors.New("qoder UMID component is not Mach-O")
		}
		if file.Magic != macho.Magic64 || file.Cpu != machoCPU(arch) || file.Type != macho.TypeExec {
			return errors.New("qoder UMID Mach-O does not match this platform")
		}
		return nil
	}
	file, err := elf.NewFile(r)
	if err != nil {
		return errors.New("qoder UMID component is not ELF")
	}
	if file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB || file.Machine != elfMachine(arch) || (file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) {
		return errors.New("qoder UMID ELF does not match this platform")
	}
	return nil
}
