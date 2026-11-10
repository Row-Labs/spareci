package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf16"
)

// The worker sets ACTIONS_RESULTS_URL for every step, overwriting anything we
// put in the environment. Renaming that one string in the worker (same length,
// so nothing else moves) makes it set ACTIONS_RESULTS_ORL instead, and our
// value reaches actions/cache. Other self-hosted cache servers use the same
// patch. If a runner release changes the string, the patch fails and jobs use
// GitHub's cache as before.
var (
	resultsVar = utf16le("ACTIONS_RESULTS_URL")
	renamedVar = utf16le("ACTIONS_RESULTS_ORL")
)

func utf16le(s string) []byte {
	var b bytes.Buffer
	for _, r := range utf16.Encode([]rune(s)) {
		b.WriteByte(byte(r))
		b.WriteByte(byte(r >> 8))
	}
	return b.Bytes()
}

// PatchWorker rewrites the worker in an install dir so a cache server can
// supply ACTIONS_RESULTS_URL. It is idempotent.
func PatchWorker(installDir string) error {
	path := filepath.Join(installDir, "bin", "Runner.Worker.dll")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	n := bytes.Count(data, resultsVar)
	if n == 0 {
		if bytes.Contains(data, renamedVar) {
			return nil // already patched
		}
		return fmt.Errorf("%s does not contain ACTIONS_RESULTS_URL; this runner version is not supported by the local cache", path)
	}
	data = bytes.ReplaceAll(data, resultsVar, renamedVar)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PrepareCacheVariant makes (once) a patched copy of the install for use with
// the local cache, leaving the original untouched for --no-cache.
func (h *Host) PrepareCacheVariant() error {
	src, dst := h.installDir(), h.installDir()+"-cache"
	if _, err := os.Stat(filepath.Join(dst, "run.sh")); err != nil {
		tmp := dst + ".partial"
		_ = os.RemoveAll(tmp)
		if err := clone(src, tmp); err != nil {
			return err
		}
		if err := PatchWorker(tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
	}
	h.source = dst
	return nil
}
