package builtin

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	searchToolNetworkTimeout  = 10 * time.Second
	searchToolDownloadTimeout = 120 * time.Second
)

var errUnsupportedManagedSearchToolPlatform = errors.New("unsupported managed search tool platform")

var managedSearchToolMu sync.Mutex

func fdSearchToolDependency() searchToolDependency {
	return searchToolDependency{
		key:         "fd",
		displayName: "fd",
		binaryName:  "fd",
		candidates:  []string{"fd", "fdfind"},
		repo:        "sharkdp/fd",
		tagPrefix:   "v",
		assetName:   fdAssetName,
		termuxPkg:   "fd",
	}
}

func ripgrepSearchToolDependency() searchToolDependency {
	return searchToolDependency{
		key:         "rg",
		displayName: "ripgrep (rg)",
		binaryName:  "rg",
		candidates:  []string{"rg"},
		repo:        "BurntSushi/ripgrep",
		assetName:   ripgrepAssetName,
		termuxPkg:   "ripgrep",
	}
}

func downloadManagedSearchTool(ctx context.Context, dependency searchToolDependency) (string, error) {
	managedSearchToolMu.Lock()
	defer managedSearchToolMu.Unlock()

	if command := managedSearchToolPath(dependency); command != "" {
		return command, nil
	}

	version, err := latestSearchToolVersion(ctx, dependency.repo)
	if err != nil {
		return "", err
	}
	if dependency.key == "fd" && runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		version = "10.3.0"
	}

	assetName := dependency.assetName(version, runtime.GOOS, runtime.GOARCH)
	if assetName == "" {
		return "", fmt.Errorf("%w: %s/%s", errUnsupportedManagedSearchToolPlatform, runtime.GOOS, runtime.GOARCH)
	}

	binDir, err := searchToolBinDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}

	downloadURL := fmt.Sprintf(
		"https://github.com/%s/releases/download/%s%s/%s",
		dependency.repo,
		dependency.tagPrefix,
		version,
		assetName,
	)
	archivePath := filepath.Join(binDir, assetName)
	if err := downloadSearchToolArchive(ctx, downloadURL, archivePath); err != nil {
		return "", err
	}
	defer os.Remove(archivePath)

	extractDir, err := os.MkdirTemp(binDir, "extract_tmp_"+dependency.binaryName+"_")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(extractDir)

	if err := extractSearchToolArchive(archivePath, extractDir, assetName); err != nil {
		return "", err
	}

	binaryPath, err := findManagedBinary(extractDir, binaryFileName(dependency.binaryName))
	if err != nil {
		return "", err
	}

	targetPath := filepath.Join(binDir, binaryFileName(dependency.binaryName))
	_ = os.Remove(targetPath)
	if err := os.Rename(binaryPath, targetPath); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(targetPath, 0o755); err != nil {
			return "", err
		}
	}
	return targetPath, nil
}

func latestSearchToolVersion(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, searchToolNetworkTimeout)
	defer cancel()

	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "along-coding-agent")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub API error: %d", resp.StatusCode)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	version := strings.TrimPrefix(payload.TagName, "v")
	if version == "" {
		return "", errors.New("latest release response missing tag_name")
	}
	return version, nil
}

func downloadSearchToolArchive(ctx context.Context, url string, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, searchToolDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("failed to download: %d", resp.StatusCode)
	}

	file, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, resp.Body); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func extractSearchToolArchive(archivePath string, extractDir string, assetName string) error {
	switch {
	case strings.HasSuffix(assetName, ".tar.gz"):
		return extractTarGzSearchToolArchive(archivePath, extractDir)
	case strings.HasSuffix(assetName, ".zip"):
		return extractZipSearchToolArchive(archivePath, extractDir)
	default:
		return fmt.Errorf("unsupported archive format: %s", assetName)
	}
}

func extractTarGzSearchToolArchive(archivePath string, extractDir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeArchivePath(extractDir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, header.FileInfo().Mode())
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, reader); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		}
	}
}

func extractZipSearchToolArchive(archivePath string, extractDir string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	for _, file := range reader.File {
		target, err := safeArchivePath(extractDir, file.Name)
		if err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, file.FileInfo().Mode())
		if err != nil {
			_ = src.Close()
			return err
		}
		if _, err := io.Copy(dst, src); err != nil {
			_ = src.Close()
			_ = dst.Close()
			return err
		}
		if err := src.Close(); err != nil {
			_ = dst.Close()
			return err
		}
		if err := dst.Close(); err != nil {
			return err
		}
	}
	return nil
}

func safeArchivePath(root string, name string) (string, error) {
	cleanName := filepath.Clean(name)
	if filepath.IsAbs(cleanName) || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive path escapes destination: %s", name)
	}
	target := filepath.Join(root, cleanName)
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive path escapes destination: %s", name)
	}
	return target, nil
}

func findManagedBinary(root string, binary string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != binary {
			return nil
		}
		found = path
		return filepath.SkipAll
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("binary not found in archive: expected %s", binary)
	}
	return found, nil
}

func binaryFileName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func fdAssetName(version string, goos string, goarch string) string {
	arch, ok := releaseArch(goarch)
	if !ok {
		return ""
	}
	switch goos {
	case "darwin":
		return fmt.Sprintf("fd-v%s-%s-apple-darwin.tar.gz", version, arch)
	case "linux":
		return fmt.Sprintf("fd-v%s-%s-unknown-linux-gnu.tar.gz", version, arch)
	case "windows":
		return fmt.Sprintf("fd-v%s-%s-pc-windows-msvc.zip", version, arch)
	default:
		return ""
	}
}

func ripgrepAssetName(version string, goos string, goarch string) string {
	arch, ok := releaseArch(goarch)
	if !ok {
		return ""
	}
	switch goos {
	case "darwin":
		return fmt.Sprintf("ripgrep-%s-%s-apple-darwin.tar.gz", version, arch)
	case "linux":
		if goarch == "arm64" {
			return fmt.Sprintf("ripgrep-%s-aarch64-unknown-linux-gnu.tar.gz", version)
		}
		return fmt.Sprintf("ripgrep-%s-%s-unknown-linux-musl.tar.gz", version, arch)
	case "windows":
		return fmt.Sprintf("ripgrep-%s-%s-pc-windows-msvc.zip", version, arch)
	default:
		return ""
	}
}

func releaseArch(goarch string) (string, bool) {
	switch goarch {
	case "amd64":
		return "x86_64", true
	case "arm64":
		return "aarch64", true
	default:
		return "", false
	}
}
