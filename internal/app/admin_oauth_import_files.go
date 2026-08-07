package app

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"path"
	"slices"
	"strings"
)

const (
	maxOAuthCredentialImportBytes        = 1 << 20
	maxOAuthCredentialArchiveBytes       = 8 << 20
	maxOAuthCredentialImportRequestBytes = 32 << 20
	maxOAuthCredentialArchiveEntries     = 1000
	maxOAuthCredentialExpandedBytes      = 32 << 20
)

type oauthCredentialImportFile struct {
	FileName string
	SortName string
	Raw      []byte
	Priority int
	Err      error
}

type oauthCredentialImportBudget struct {
	entries      int
	expandedSize uint64
}

func readOAuthCredentialUpload(file *multipart.FileHeader, maxBytes int64) ([]byte, error) {
	if file == nil || file.Size <= 0 || file.Size > maxBytes {
		return nil, errors.New("credential file size is invalid")
	}
	opened, err := file.Open()
	if err != nil {
		return nil, errors.New("open credential file failed")
	}
	raw, readErr := io.ReadAll(io.LimitReader(opened, maxBytes+1))
	closeErr := opened.Close()
	if readErr != nil || int64(len(raw)) > maxBytes {
		return nil, errors.New("failed to read credential")
	}
	if closeErr != nil {
		return nil, errors.New("close credential file failed")
	}
	return raw, nil
}

func expandOAuthCredentialUploads(files []*multipart.FileHeader) []oauthCredentialImportFile {
	items := make([]oauthCredentialImportFile, 0, len(files))
	budget := oauthCredentialImportBudget{}
	for _, file := range files {
		fileName := ""
		if file != nil {
			fileName = file.Filename
		}
		archiveType := oauthCredentialArchiveType(fileName)
		maxBytes := int64(maxOAuthCredentialImportBytes)
		if archiveType != "" {
			maxBytes = maxOAuthCredentialArchiveBytes
		}
		raw, err := readOAuthCredentialUpload(file, maxBytes)
		if err != nil {
			items = append(items, failedOAuthCredentialImportFile(fileName, err))
			continue
		}

		var expanded []oauthCredentialImportFile
		switch archiveType {
		case "zip":
			expanded, err = expandOAuthCredentialZIP(fileName, raw, &budget)
		case "tar.gz":
			expanded, err = expandOAuthCredentialTarGz(fileName, raw, &budget)
		default:
			err = budget.reserve(uint64(len(raw)))
			if err == nil {
				expanded = []oauthCredentialImportFile{newOAuthCredentialImportFile(fileName, fileName, raw)}
			}
		}
		if err != nil {
			items = append(items, failedOAuthCredentialImportFile(fileName, err))
			continue
		}
		if len(expanded) == 0 {
			items = append(items, failedOAuthCredentialImportFile(fileName, errors.New("credential archive contains no JSON files")))
			continue
		}
		items = append(items, expanded...)
	}
	sortOAuthCredentialImportFiles(items)
	return items
}

func oauthCredentialArchiveType(fileName string) string {
	lowerName := strings.ToLower(strings.TrimSpace(fileName))
	switch {
	case strings.HasSuffix(lowerName, ".zip"):
		return "zip"
	case strings.HasSuffix(lowerName, ".tar.gz"), strings.HasSuffix(lowerName, ".tgz"):
		return "tar.gz"
	default:
		return ""
	}
}

func (b *oauthCredentialImportBudget) reserve(size uint64) error {
	if b.entries >= maxOAuthCredentialArchiveEntries {
		return fmt.Errorf("credential import exceeds %d entries", maxOAuthCredentialArchiveEntries)
	}
	b.entries++
	remaining := uint64(maxOAuthCredentialExpandedBytes) - b.expandedSize
	if size > remaining {
		b.expandedSize = maxOAuthCredentialExpandedBytes
		return fmt.Errorf("credential import exceeds %d expanded bytes", maxOAuthCredentialExpandedBytes)
	}
	b.expandedSize += size
	return nil
}

func expandOAuthCredentialZIP(archiveName string, raw []byte, budget *oauthCredentialImportBudget) ([]oauthCredentialImportFile, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("open ZIP credential archive: %w", err)
	}
	items := make([]oauthCredentialImportFile, 0, len(reader.File))
	for _, entry := range reader.File {
		if err := budget.reserve(entry.UncompressedSize64); err != nil {
			return nil, err
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if !entry.Mode().IsRegular() {
			return nil, fmt.Errorf("ZIP credential archive entry %q is not a regular file", entry.Name)
		}
		if err := validateOAuthCredentialArchivePath(entry.Name); err != nil {
			return nil, err
		}
		if !strings.EqualFold(path.Ext(entry.Name), ".json") {
			continue
		}
		if entry.UncompressedSize64 > maxOAuthCredentialImportBytes {
			return nil, fmt.Errorf("credential archive entry %q exceeds %d bytes", entry.Name, maxOAuthCredentialImportBytes)
		}
		entryRaw, err := readOAuthCredentialZIPEntry(entry, budget)
		if err != nil {
			return nil, err
		}
		items = append(items, newOAuthCredentialImportFile(
			archiveName+"/"+entry.Name,
			entry.Name,
			entryRaw,
		))
	}
	return items, nil
}

func readOAuthCredentialZIPEntry(entry *zip.File, budget *oauthCredentialImportBudget) ([]byte, error) {
	opened, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open ZIP credential archive entry %q: %w", entry.Name, err)
	}
	remaining := uint64(maxOAuthCredentialExpandedBytes) - budget.expandedSize
	maxActualSize := entry.UncompressedSize64 + remaining
	if maxActualSize > maxOAuthCredentialImportBytes {
		maxActualSize = maxOAuthCredentialImportBytes
	}
	raw, readErr := io.ReadAll(io.LimitReader(opened, int64(maxActualSize)+1))
	closeErr := opened.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read ZIP credential archive entry %q: %w", entry.Name, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close ZIP credential archive entry %q: %w", entry.Name, closeErr)
	}
	if uint64(len(raw)) > maxActualSize || len(raw) > maxOAuthCredentialImportBytes {
		budget.expandedSize = maxOAuthCredentialExpandedBytes
		return nil, fmt.Errorf("credential archive entry %q exceeds import limits", entry.Name)
	}
	if actualSize := uint64(len(raw)); actualSize > entry.UncompressedSize64 {
		budget.expandedSize += actualSize - entry.UncompressedSize64
	}
	return raw, nil
}

func expandOAuthCredentialTarGz(archiveName string, raw []byte, budget *oauthCredentialImportBudget) ([]oauthCredentialImportFile, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("open tar.gz credential archive: %w", err)
	}
	items, readErr := readOAuthCredentialTar(archiveName, tar.NewReader(gzipReader), budget)
	closeErr := gzipReader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close tar.gz credential archive: %w", closeErr)
	}
	return items, nil
}

func readOAuthCredentialTar(archiveName string, reader *tar.Reader, budget *oauthCredentialImportBudget) ([]oauthCredentialImportFile, error) {
	items := make([]oauthCredentialImportFile, 0)
	for {
		entry, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return items, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tar.gz credential archive: %w", err)
		}
		if entry.Size < 0 {
			return nil, fmt.Errorf("tar.gz credential archive entry %q has invalid size", entry.Name)
		}
		if err := budget.reserve(uint64(entry.Size)); err != nil {
			return nil, err
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if !entry.FileInfo().Mode().IsRegular() {
			return nil, fmt.Errorf("tar.gz credential archive entry %q is not a regular file", entry.Name)
		}
		if err := validateOAuthCredentialArchivePath(entry.Name); err != nil {
			return nil, err
		}
		if !strings.EqualFold(path.Ext(entry.Name), ".json") {
			continue
		}
		if entry.Size > maxOAuthCredentialImportBytes {
			return nil, fmt.Errorf("credential archive entry %q exceeds %d bytes", entry.Name, maxOAuthCredentialImportBytes)
		}
		entryRaw, err := io.ReadAll(io.LimitReader(reader, maxOAuthCredentialImportBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read tar.gz credential archive entry %q: %w", entry.Name, err)
		}
		if len(entryRaw) > maxOAuthCredentialImportBytes {
			return nil, fmt.Errorf("credential archive entry %q exceeds %d bytes", entry.Name, maxOAuthCredentialImportBytes)
		}
		items = append(items, newOAuthCredentialImportFile(
			archiveName+"/"+entry.Name,
			entry.Name,
			entryRaw,
		))
	}
}

func validateOAuthCredentialArchivePath(name string) error {
	if name == "" || path.IsAbs(name) || strings.ContainsRune(name, '\\') {
		return fmt.Errorf("credential archive entry path %q is invalid", name)
	}
	cleanName := path.Clean(name)
	if cleanName != name || cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, "../") {
		return fmt.Errorf("credential archive entry path %q is invalid", name)
	}
	return nil
}

func newOAuthCredentialImportFile(fileName, sortName string, raw []byte) oauthCredentialImportFile {
	priority, err := parseOAuthCredentialPriority(raw)
	return oauthCredentialImportFile{
		FileName: fileName,
		SortName: sortName,
		Raw:      raw,
		Priority: priority,
		Err:      err,
	}
}

func failedOAuthCredentialImportFile(fileName string, err error) oauthCredentialImportFile {
	return oauthCredentialImportFile{FileName: fileName, SortName: fileName, Err: err}
}

func sortOAuthCredentialImportFiles(files []oauthCredentialImportFile) {
	slices.SortStableFunc(files, func(a, b oauthCredentialImportFile) int {
		if a.Priority < b.Priority {
			return -1
		}
		if a.Priority > b.Priority {
			return 1
		}
		if order := strings.Compare(strings.ToLower(a.SortName), strings.ToLower(b.SortName)); order != 0 {
			return order
		}
		if order := strings.Compare(a.SortName, b.SortName); order != 0 {
			return order
		}
		return strings.Compare(a.FileName, b.FileName)
	})
}
