// Package packman resolves, caches, and pins remote pack imports.
package packman

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/config"
)

var runGit = defaultRunGit

// RepoCacheRoot returns the shared machine-local cache root for URL+commit clones.
func RepoCacheRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".gc", "cache", "repos"), nil
}

// RepoCacheKey returns the sha256(url+commit) cache key.
// Delegates to config.RepoCacheKey for canonical normalization so
// the loader and packman always agree on cache paths.
func RepoCacheKey(source, commit string) string {
	return config.RepoCacheKey(source, commit)
}

// RepoCachePath returns the cache path for a specific source+commit pair.
func RepoCachePath(source, commit string) (string, error) {
	root, err := RepoCacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, RepoCacheKey(source, commit)), nil
}

// EnsureRepoInCache clones and checks out the requested commit when absent,
// or repairs an existing cache whose checkout has drifted from the lock entry.
func EnsureRepoInCache(source, commit string) (string, error) {
	parsed := normalizeRemoteSource(source)
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		return "", err
	}
	root, err := RepoCacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("creating repo cache root: %w", err)
	}
	return withRepoCacheWriteLock(root, func() (string, error) {
		return ensureRepoInCacheLocked(source, commit, parsed, cachePath)
	})
}

func ensureRepoInCacheLocked(source, commit string, parsed remoteSource, cachePath string) (string, error) {
	if _, err := os.Stat(filepath.Join(cachePath, ".git")); err == nil {
		if err := checkoutExistingCache(cachePath, commit); err == nil {
			if err := validateCachedPackRoot(source, cachePath); err != nil {
				if removeErr := os.RemoveAll(cachePath); removeErr != nil {
					return "", fmt.Errorf("removing invalid repo cache %q after %v: %w", cachePath, err, removeErr)
				}
			} else {
				return cachePath, nil
			}
		} else if err := os.RemoveAll(cachePath); err != nil {
			return "", fmt.Errorf("removing stale repo cache %q: %w", cachePath, err)
		}
	} else if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		if _, statErr := os.Stat(cachePath); statErr == nil {
			if removeErr := os.RemoveAll(cachePath); removeErr != nil {
				return "", fmt.Errorf("removing invalid repo cache %q: %w", cachePath, removeErr)
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("checking repo cache %q: %w", cachePath, statErr)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("checking repo cache %q: %w", cachePath, err)
	}

	if _, err := runGit("", "clone", "--quiet", parsed.CloneURL, cachePath); err != nil {
		return "", fmt.Errorf("cloning %q: %w", source, err)
	}
	if _, err := runGit(cachePath, "checkout", "--quiet", commit); err != nil {
		return "", fmt.Errorf("checking out %q: %w", commit, err)
	}
	if err := validateCachedPackRoot(source, cachePath); err != nil {
		return "", err
	}
	return cachePath, nil
}

const repoCacheLockName = ".packman-cache.lock"

func withRepoCacheWriteLock(root string, fn func() (string, error)) (string, error) {
	return withRepoCacheLock(root, syscall.LOCK_EX, fn)
}

func withRepoCacheReadLock(fn func() error) error {
	root, err := RepoCacheRoot()
	if err != nil {
		return err
	}
	lockFile, err := os.OpenFile(filepath.Join(root, repoCacheLockName), os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return fn()
		}
		return fmt.Errorf("opening repo cache lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("locking repo cache: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

func withRepoCacheLock(root string, mode int, fn func() (string, error)) (string, error) {
	lockFile, err := os.OpenFile(filepath.Join(root, repoCacheLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return "", fmt.Errorf("opening repo cache lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), mode); err != nil {
		return "", fmt.Errorf("locking repo cache: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

func checkoutExistingCache(cachePath, commit string) error {
	head, headErr := runGit(cachePath, "rev-parse", "HEAD")
	if headErr == nil && sameCommit(head, commit) {
		return nil
	}
	if _, err := runGit(cachePath, "checkout", "--quiet", commit); err != nil {
		if headErr != nil {
			return fmt.Errorf("reading cached repo HEAD: %w; checking out %q: %v", headErr, commit, err)
		}
		return fmt.Errorf("checking out %q in cached repo: %w", commit, err)
	}
	return nil
}

func validateCachedPackRoot(source, cachePath string) error {
	packPath := filepath.Join(cachedPackDir(source, cachePath), "pack.toml")
	st, err := os.Stat(packPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cached pack %q is missing pack.toml at %s", source, packPath)
		}
		return fmt.Errorf("checking cached pack %q at %s: %w", source, packPath, err)
	}
	if st.IsDir() {
		return fmt.Errorf("cached pack %q has directory where pack.toml is expected at %s", source, packPath)
	}
	return nil
}

type remoteSource struct {
	CloneURL string
	Subpath  string
}

func normalizeRemoteSource(source string) remoteSource {
	if strings.Contains(source, "github.com/") && strings.Contains(source, "/tree/") {
		return parseGitHubTreeSource(source)
	}
	if strings.HasPrefix(source, "github.com/") {
		return remoteSource{CloneURL: "https://" + source}
	}
	return parsePackmanRemoteSource(source)
}

func parsePackmanRemoteSource(source string) remoteSource {
	withoutRef := source
	if i := strings.LastIndex(withoutRef, "#"); i >= 0 {
		withoutRef = withoutRef[:i]
	}

	searchFrom := 0
	if idx := strings.Index(withoutRef, "://"); idx >= 0 {
		searchFrom = idx + 3
	}
	if i := strings.Index(withoutRef[searchFrom:], "//"); i >= 0 {
		pos := searchFrom + i
		return remoteSource{
			CloneURL: withoutRef[:pos],
			Subpath:  withoutRef[pos+2:],
		}
	}
	return remoteSource{CloneURL: withoutRef}
}

func parseGitHubTreeSource(source string) remoteSource {
	u := source
	scheme := ""
	if idx := strings.Index(u, "://"); idx >= 0 {
		scheme = u[:idx+3]
		u = u[idx+3:]
	}
	parts := strings.SplitN(u, "/", 6)
	if len(parts) < 5 {
		return remoteSource{CloneURL: source}
	}
	cloneURL := scheme + parts[0] + "/" + parts[1] + "/" + parts[2] + ".git"
	if len(parts) > 5 {
		return remoteSource{CloneURL: cloneURL, Subpath: parts[5]}
	}
	return remoteSource{CloneURL: cloneURL}
}

func defaultRunGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && fetchGitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

var fetchGitEnvBlacklist = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
}
