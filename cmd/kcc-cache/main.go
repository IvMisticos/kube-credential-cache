package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

type CacheFile struct {
	Credentials map[string]ClientAuthentication `json:"credentials"`
}

// https://kubernetes.io/docs/reference/config-api/client-authentication.v1
type ClientAuthentication struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     struct {
		ExpirationTimestamp   time.Time `json:"expirationTimestamp"`
		Token                 string    `json:"token,omitempty"`
		ClientCertificateData string    `json:"clientCertificateData,omitempty"`
		ClientKeyData         string    `json:"clientKeyData,omitempty"`
	} `json:"status"`
}

// keyringService is the service name used for entries stored in the OS secret store.
const keyringService = "kube-credential-cache"

// Backend abstracts where cached credentials are persisted between invocations.
//
// kcc-cache is a short-lived process (spawned by kubectl on every API call), so
// there is no in-process memory that survives between calls. A Backend therefore
// persists credentials somewhere external: either the OS secret store ("keyring")
// or a plaintext JSON file ("file").
type Backend interface {
	// Get returns the cached credential for key. ok is false when there is no
	// (valid) entry for key.
	Get(key string) (cred ClientAuthentication, ok bool, err error)
	// Set stores cred under key.
	Set(key string, cred ClientAuthentication) error
}

func main() {
	// help
	//
	// kcc-cache wraps another command (os.Args[1:]) and execs it on a cache
	// miss. Without this guard, `kcc-cache -h` would try to exec a binary
	// literally named "-h" and fail with a confusing PATH error, so handle the
	// usual help flags explicitly before treating the first arg as a command.
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		os.Exit(0)
	}

	// configuration
	var (
		refreshMargin   = time.Second * 30
		cacheKeyEnvlist = []string{"KUBE_CREDENTIAL_CACHE_USER", "AWS_PROFILE", "AWS_REGION", "AWS_VAULT"}
	)
	if e := os.Getenv("KUBE_CREDENTIAL_CACHE_REFRESH_MARGIN"); e != "" {
		d, err := time.ParseDuration(e)
		if err != nil {
			fatal("invalid environment variable 'KUBE_CREDENTIAL_CACHE_REFRESH_MARGIN': %s", err.Error())
		}
		refreshMargin = d
	}
	if e := os.Getenv("KUBE_CREDENTIAL_CACHE_CACHEKEY_ENV_LIST"); e != "" {
		cacheKeyEnvlist = strings.Split(e, ",")
	}

	// cache key
	//
	// When kubectl provides cluster info (provideClusterInfo: true, which
	// kcc-injector enables), the target API server URL is available via the
	// KUBERNETES_EXEC_INFO env var. Combined with the kubeconfig user name
	// (KUBE_CREDENTIAL_CACHE_USER, also set by kcc-injector) this yields a
	// stable cache key: it survives cosmetic argv changes and never serves one
	// identity's credentials for another identity on the same cluster.
	//
	// If the server URL is unavailable (provideClusterInfo not set, or a manual
	// setup), fall back to the legacy argv + env based key.
	var cacheKey string
	{
		server := ""
		if e := os.Getenv("KUBERNETES_EXEC_INFO"); e != "" {
			var execInfo struct {
				Spec struct {
					Cluster struct {
						Server string `json:"server"`
					} `json:"cluster"`
				} `json:"spec"`
			}
			if err := json.Unmarshal([]byte(e), &execInfo); err == nil {
				server = execInfo.Spec.Cluster.Server
			}
		}

		if server != "" {
			cacheKey = fmt.Sprintf("user=%q server=%q", os.Getenv("KUBE_CREDENTIAL_CACHE_USER"), server)
		} else {
			cacheKey = strings.Join(os.Args[1:], " ")
			env := ""
			for _, key := range cacheKeyEnvlist {
				v := os.Getenv(key)
				if v == "" {
					continue
				}
				env = fmt.Sprintf("%s %s='%s'", env, key, v)
			}
			if env != "" {
				cacheKey = fmt.Sprintf("%s # env:%s", cacheKey, env)
			}
		}
	}

	// select storage backend
	backend := newBackend(os.Getenv("KUBE_CREDENTIAL_CACHE_BACKEND"))

	// check cache
	cache, ok, err := backend.Get(cacheKey)
	if err != nil {
		fatal("cache read failed: %s", err)
	}
	if !ok || time.Until(cache.Status.ExpirationTimestamp) < refreshMargin {
		// refresh (os.Args[1] is guaranteed present; checked at startup)
		cmd := exec.Command(os.Args[1], os.Args[2:]...)
		cmd.Stderr = os.Stderr
		bytes, err := cmd.Output()

		if err != nil {
			if len(bytes) > 0 {
				fatal("read command output failed: %s\nactual stdout: %s", err, string(bytes))
			}
			fatal("read command output failed: %s", err)
		}

		if len(bytes) == 0 {
			fatal("empty stdout, but without error")
		}

		cache = ClientAuthentication{}
		if err := json.Unmarshal(bytes, &cache); err != nil {
			fatal("json.Unmarshal() failed(read command output): %s\nactual stdout: %s", err, string(bytes))
		}

		if err := backend.Set(cacheKey, cache); err != nil {
			fatal("cache write failed: %s", err)
		}
	}

	// print
	output, err := json.Marshal(cache)
	if err != nil {
		fatal("json.Marshal() failed: %s", err)
	}
	fmt.Println(string(output))
}

// newBackend selects a storage backend.
//
//	"keyring" -> OS secret store (macOS Keychain / Linux Secret Service / Windows Credential Manager)
//	"file"    -> plaintext JSON cache file
//	""        -> keyring if the OS secret store is reachable, otherwise file (for headless/CI hosts)
func newBackend(name string) Backend {
	switch name {
	case "file":
		return fileBackend{path: resolveCacheFilepath()}
	case "keyring":
		return keyringBackend{}
	case "":
		if keyringAvailable() {
			return keyringBackend{}
		}
		return fileBackend{path: resolveCacheFilepath()}
	default:
		fatal("invalid environment variable 'KUBE_CREDENTIAL_CACHE_BACKEND': %q (expected \"keyring\" or \"file\")", name)
		return nil // unreachable
	}
}

// keyringAvailable reports whether the OS secret store can be reached. The probe
// is non-destructive: a working store returns ErrNotFound for a missing entry,
// while an unavailable store (e.g. no Secret Service / D-Bus) returns another error.
func keyringAvailable() bool {
	_, err := keyring.Get(keyringService, "__kcc_probe__")
	return err == nil || errors.Is(err, keyring.ErrNotFound)
}

// keyringBackend stores each credential as a JSON value in the OS secret store,
// keyed by the cache key. Nothing is written to disk in plaintext.
type keyringBackend struct{}

func (keyringBackend) Get(key string) (ClientAuthentication, bool, error) {
	s, err := keyring.Get(keyringService, key)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return ClientAuthentication{}, false, nil
		}
		return ClientAuthentication{}, false, err
	}
	var cred ClientAuthentication
	if err := json.Unmarshal([]byte(s), &cred); err != nil {
		// treat corruption as a cache miss; it will be overwritten on refresh
		log("json.Unmarshal() failed(read keyring entry): %s\n...Corruption detected, refreshing credential", err)
		return ClientAuthentication{}, false, nil
	}
	return cred, true, nil
}

func (keyringBackend) Set(key string, cred ClientAuthentication) error {
	bytes, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return keyring.Set(keyringService, key, string(bytes))
}

// fileBackend stores all credentials as a single plaintext JSON file, protected
// only by filesystem permissions (0600 file, 0700 directory).
type fileBackend struct {
	path string
}

func (b fileBackend) load() (CacheFile, error) {
	cf := CacheFile{Credentials: map[string]ClientAuthentication{}}
	bytes, err := os.ReadFile(b.path)
	if err != nil {
		if os.IsNotExist(err) {
			return cf, nil
		}
		return cf, err
	}
	if len(bytes) > 0 {
		if err := json.Unmarshal(bytes, &cf); err != nil {
			// recreate on corruption, matching previous behaviour
			log("json.Unmarshal() failed(read cache file): %s\n...Corruption detected, recreate cache file", err)
			return CacheFile{Credentials: map[string]ClientAuthentication{}}, nil
		}
	}
	if cf.Credentials == nil {
		cf.Credentials = map[string]ClientAuthentication{}
	}
	return cf, nil
}

func (b fileBackend) Get(key string) (ClientAuthentication, bool, error) {
	cf, err := b.load()
	if err != nil {
		return ClientAuthentication{}, false, err
	}
	cred, ok := cf.Credentials[key]
	return cred, ok, nil
}

func (b fileBackend) Set(key string, cred ClientAuthentication) error {
	cf, err := b.load()
	if err != nil {
		return err
	}

	// cleanup expired entries
	for k, v := range cf.Credentials {
		if time.Now().After(v.Status.ExpirationTimestamp) {
			delete(cf.Credentials, k)
		}
	}
	cf.Credentials[key] = cred

	bytes, err := json.Marshal(cf)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path.Dir(b.path), 0700); err != nil {
		return fmt.Errorf("mkdir failed: %w", err)
	}
	return os.WriteFile(b.path, bytes, 0600)
}

// resolveCacheFilepath returns the path of the plaintext cache file used by the
// file backend.
func resolveCacheFilepath() string {
	if e := os.Getenv("KUBE_CREDENTIAL_CACHE_FILE"); e != "" {
		return e
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		fatal("can't find CacheDir. fix error or set 'KUBE_CREDENTIAL_CACHE_FILE': %s", err)
	}
	return path.Join(cacheDir, "kube-credential-cache", "cache.json")
}

// usage prints how to invoke kcc-cache. It is a transparent caching wrapper:
// the arguments after the program name are the credential command it runs and
// caches, so there are no flags of its own (configuration is via environment
// variables, documented in the README).
func usage(w *os.File) {
	name := path.Base(os.Args[0])
	// Best-effort write; there is nothing useful to do if emitting usage fails.
	_, _ = fmt.Fprintf(w, `Usage: %[1]s <command> [args...]

Caching proxy for Kubernetes client-go credential plugins (ExecCredential).
Runs <command> [args...], caches its ExecCredential output, and serves it
from cache until the credential is near expiry.

Example:
  %[1]s aws --region <region> eks get-token --cluster-name <cluster>

Configuration is via environment variables; see
https://github.com/ryodocx/kube-credential-cache#kcc-cache
`, name)
}

func fatal(format string, v ...any) {
	log(format, v...)

	var commit = "main"
	if i, ok := debug.ReadBuildInfo(); ok {
		for _, v := range i.Settings {
			if v.Key == "vcs.revision" {
				commit = v.Value
			}
		}
	}
	_, _, line, _ := runtime.Caller(1)
	fmt.Fprintf(os.Stderr, "error occurred at: https://github.com/ryodocx/kube-credential-cache/blob/%s/cmd/kcc-cache/main.go#L%d\n", commit, line)

	os.Exit(1)
}

func log(format string, v ...any) {
	fmt.Fprintf(os.Stderr, "%s: ", path.Base(os.Args[0]))
	fmt.Fprintf(os.Stderr, format+"\n", v...)
}
