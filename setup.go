package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type SetupCmd struct {
	Force bool `help:"Re-download even if files exist." short:"f"`
}

const (
	onnxRuntimeVersion = "1.22.0"
	modelName          = "sentence-transformers/all-MiniLM-L6-v2"
	embeddingDim       = 384
)

func modelsDir() string {
	return filepath.Join(expandPath("~/.obliscence"), "models")
}

func onnxRuntimeLibPath() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(modelsDir(), "onnxruntime", fmt.Sprintf("libonnxruntime.%s.dylib", onnxRuntimeVersion))
	}
	return filepath.Join(modelsDir(), "onnxruntime", fmt.Sprintf("libonnxruntime.so.%s", onnxRuntimeVersion))
}

func onnxModelPath() string {
	return filepath.Join(modelsDir(), "all-MiniLM-L6-v2", "model.onnx")
}

func (cmd *SetupCmd) Run(rc *RunContext) error {
	dir := modelsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	if err := cmd.downloadONNXRuntime(); err != nil {
		return fmt.Errorf("downloading ONNX Runtime: %w", err)
	}

	if err := cmd.downloadModel(); err != nil {
		return fmt.Errorf("downloading model: %w", err)
	}

	fmt.Fprintln(os.Stderr, "setup complete. run 'obliscence index' to generate embeddings.")
	return nil
}

func (cmd *SetupCmd) downloadONNXRuntime() error {
	dest := onnxRuntimeLibPath()
	if !cmd.Force {
		if _, err := os.Stat(dest); err == nil {
			fmt.Fprintln(os.Stderr, "ONNX Runtime already downloaded, skipping (use --force to re-download)")
			return nil
		}
	}

	url := onnxRuntimeURL()
	if url == "" {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	fmt.Fprintf(os.Stderr, "downloading ONNX Runtime %s...\n", onnxRuntimeVersion)

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	return downloadAndExtractTgz(url, dest)
}

func (cmd *SetupCmd) downloadModel() error {
	dest := onnxModelPath()
	if !cmd.Force {
		if _, err := os.Stat(dest); err == nil {
			fmt.Fprintln(os.Stderr, "model already downloaded, skipping (use --force to re-download)")
			return nil
		}
	}

	url := "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/onnx/model.onnx"
	fmt.Fprintln(os.Stderr, "downloading all-MiniLM-L6-v2 ONNX model...")

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	return downloadFile(url, dest)
}

func onnxRuntimeURL() string {
	base := fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s", onnxRuntimeVersion)
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return fmt.Sprintf("%s/onnxruntime-osx-arm64-%s.tgz", base, onnxRuntimeVersion)
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		return fmt.Sprintf("%s/onnxruntime-osx-x86_64-%s.tgz", base, onnxRuntimeVersion)
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return fmt.Sprintf("%s/onnxruntime-linux-x64-%s.tgz", base, onnxRuntimeVersion)
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		return fmt.Sprintf("%s/onnxruntime-linux-aarch64-%s.tgz", base, onnxRuntimeVersion)
	default:
		return ""
	}
}

func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}

	fmt.Fprintf(os.Stderr, "  downloaded %s (%s)\n", filepath.Base(dest), formatBytes(written))
	return os.Rename(tmp, dest)
}

// downloadAndExtractTgz downloads a .tgz and extracts the shared library from it.
func downloadAndExtractTgz(url, destLib string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	// Look for the versioned shared library in the archive (not the symlink).
	var libPattern string
	if runtime.GOOS == "darwin" {
		libPattern = fmt.Sprintf("libonnxruntime.%s.dylib", onnxRuntimeVersion)
	} else {
		libPattern = fmt.Sprintf("libonnxruntime.so.%s", onnxRuntimeVersion)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if strings.HasSuffix(hdr.Name, libPattern) && hdr.Typeflag == tar.TypeReg {
			tmp := destLib + ".tmp"
			f, err := os.Create(tmp)
			if err != nil {
				return err
			}
			written, err := io.Copy(f, tr)
			f.Close()
			if err != nil {
				os.Remove(tmp)
				return err
			}
			fmt.Fprintf(os.Stderr, "  extracted %s (%s)\n", filepath.Base(destLib), formatBytes(written))
			return os.Rename(tmp, destLib)
		}
	}

	return fmt.Errorf("shared library not found in archive")
}
