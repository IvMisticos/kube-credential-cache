# kcc: kube-credential-cache

[![lint](https://github.com/IvMisticos/kube-credential-cache/actions/workflows/golangci-lint.yaml/badge.svg)](https://github.com/IvMisticos/kube-credential-cache/actions/workflows/golangci-lint.yaml)
[![asdf-test](https://github.com/IvMisticos/kube-credential-cache/actions/workflows/asdf-test.yml/badge.svg)](https://github.com/IvMisticos/kube-credential-cache/actions/workflows/asdf-test.yml)
[![GoReleaser](https://github.com/IvMisticos/kube-credential-cache/actions/workflows/goreleaser.yaml/badge.svg)](https://github.com/IvMisticos/kube-credential-cache/actions/workflows/goreleaser.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/IvMisticos/kube-credential-cache)](https://goreportcard.com/report/github.com/IvMisticos/kube-credential-cache)

Fast access to Kubernetes!
Especially effective with kubectl + EKS, about 3~4x faster!

```sh
# first time access
$ time kubectl version &>/dev/null
kubectl version &> /dev/null  0.42s user 0.10s system 59% cpu 0.868 total

# cache effective
$ time kubectl version &>/dev/null
kubectl version &> /dev/null  0.05s user 0.02s system 24% cpu 0.308 total
```

## Architecture
[![](./docs/summary.drawio.svg)](./docs)

details is [here](./docs) (includes sequence diagram)

## Features
Work as caching proxy of [ExecCredential](https://kubernetes.io/docs/reference/config-api/client-authentication.v1/#client-authentication-k8s-io-v1-ExecCredential) object, when use [credential plugins](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#client-go-credential-plugins) of Kubernetes. (e.g. kubectl)

- kcc-cache
  - [x] Cache [ExecCredential](https://kubernetes.io/docs/reference/config-api/client-authentication.v1/#client-authentication-k8s-io-v1-ExecCredential) object
  - [x] Concern Command, Args, Env as cache-key
  - [x] Store credentials in the OS secret store (no plaintext on disk) via the `keyring` backend
  - [ ] kubeconfig automated maintenance
- kcc-injector
  - [x] kubeconfig optimize (inject kcc-cache command automatically)
  - [x] kubeconfig recovery (remove injected commands)

Benchmark with [`aws eks update-kubeconfig`](https://docs.aws.amazon.com/eks/latest/userguide/create-kubeconfig.html) (about 500ms saved per call):

![](./benchmark/graph_eks.svg)

details [here](./benchmark/)

## Installation

```sh
# Homebrew (macOS)
brew install --cask IvMisticos/tap/kube-credential-cache

# Scoop (Windows)
scoop bucket add IvMisticos https://github.com/IvMisticos/scoop-bucket
scoop install kube-credential-cache

# go install
go install github.com/IvMisticos/kube-credential-cache/cmd/kcc-cache@latest
go install github.com/IvMisticos/kube-credential-cache/cmd/kcc-injector@latest

# asdf-vm: https://asdf-vm.com
asdf plugin add kube-credential-cache

# aqua: https://aquaproj.github.io
aqua g -i IvMisticos/kube-credential-cache
```

or download from [releases](https://github.com/IvMisticos/kube-credential-cache/releases).
All methods install both `kcc-cache` and `kcc-injector`.

## Usage(edit kubeconfig)

:running: install & just run `kcc-injector -i ~/.kube/config`

:ambulance: restore kubeconfig: `kcc-injector -i -r <your kubeconfig>`


<details>
<summary>manual setup</summary>
<p>

if manually edit kubeconfig,
  * set `kcc-cache` to command
  * original command move to args
  * :warning: **Do not use the same pattern for command, args and env**
    * :warning:U sing the same pattern presents the risk of mixing up credentials
    * :warning: env is ignored if not in `KUBE_CREDENTIAL_CACHE_CACHEKEY_ENV_LIST`
    * if use `kcc-injector`, generate unique env `KUBE_CREDENTIAL_CACHE_USER` from user's name

EKS (same effect as `kcc-injector -i <your kubeconfig>`)

```diff
kind: Config
apiVersion: v1
clusters: [...]
contexts: [...]
current-context: <your-current-context>
preferences: {}
users:
  - name: user-name
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1beta1
-       command: aws
+       command: kcc-cache
        args:
+         - aws
          - --region
          - <your-region>
          - eks
          - get-token
          - --cluster-name
          - <your-cluster>
        env:
          - name: AWS_PROFILE
            value: <your-profile>
```

EKS with [aws-vault](https://github.com/99designs/aws-vault)

```diff
kind: Config
apiVersion: v1
clusters: [...]
contexts: [...]
current-context: <your-current-context>
preferences: {}
users:
  - name: user-name
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1beta1
-       command: aws
+       command: kcc-cache
        args:
+         - aws-vault
+         - exec
+         - <your-profile>
+         - --
+         - aws
          - --region
          - <your-region>
          - eks
          - get-token
          - --cluster-name
          - <your-cluster>
-       env:
-         - name: AWS_PROFILE
-           value: <your-profile>
```

kubeconfig specification
* https://kubernetes.io/docs/tasks/access-application-cluster/configure-access-multiple-clusters/
* https://pkg.go.dev/k8s.io/client-go/tools/clientcmd/api/v1#Config

</p>
</details>

## Troubleshooting

###### `error: You must be logged in to the server (the server has asked for the client to provide credentials)` at kubectl
Stale/invalid credentials may be cached (e.g. a wrong pair of aws-vault context and kubecontext, where `aws` returns an invalid credential without erroring).
Clear the cache: delete the `kube-credential-cache` entries from your OS secret store (`keyring` backend) or remove the cache file (`file` backend, path below).

###### `...Corruption detected, recreate cache file`
A broken cache file was detected. The cause is unknown; the cache is automatically recreated.

###### kubectl keeps re-running the credential plugin / prompting on every call
The credential is being written to your secret store but never served from cache.
The usual cause is a plugin that returns no `status.expirationTimestamp` (it shows
up as `"expirationTimestamp":"0001-01-01T00:00:00Z"`), so kcc-cache considers it
already expired every time. Set `KUBE_CREDENTIAL_CACHE_DEBUG=1` to see the cache
key, hit/miss and expiry decisions on stderr. The default-TTL behaviour (above)
handles this automatically; tune it with `KUBE_CREDENTIAL_CACHE_DEFAULT_TTL` and
`KUBE_CREDENTIAL_CACHE_NO_EXPIRY_THRESHOLD`.

> :information_source: In the OS secret store each cached credential is a single
> entry whose name is `kube-credential-cache:<cache-key>` (e.g.
> `kube-credential-cache:user="..." server="..."`). The `kube-credential-cache:`
> prefix is just the service name; the rest is the cache key — it is one entry,
> not two separate keys.

## Configuration

### kcc-cache

| Environment variable                    | default                                                                                                                                                                                                                                        | description                                        |
|-----------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------|
| KUBE_CREDENTIAL_CACHE_BACKEND           | _auto_ (`keyring` if the OS secret store is reachable, otherwise `file`)                                                                                                                                                                        | storage backend: `keyring` or `file`               |
| KUBE_CREDENTIAL_CACHE_FILE              | macOS:</br>`~/Library/Caches/kube-credential-cache/cache.json`</br>Linux:</br>`$XDG_CACHE_HOME/kube-credential-cache/cache.json`</br>`~/.cache/kube-credential-cache/cache.json`</br>Windows:</br>`%AppData%\kube-credential-cache\cache.json` | path of Cache file (`file` backend only)           |
| KUBE_CREDENTIAL_CACHE_REFRESH_MARGIN    | `30s`                                                                                                                                                                                                                                          | margin of credential refresh                       |
| KUBE_CREDENTIAL_CACHE_DEFAULT_TTL       | `1h`                                                                                                                                                                                                                                          | TTL applied to credentials that report no usable expiry (see below) |
| KUBE_CREDENTIAL_CACHE_NO_EXPIRY_THRESHOLD | `24h`                                                                                                                                                                                                                                        | how far in the past a credential's expiry must be to count as "no expiry" |
| KUBE_CREDENTIAL_CACHE_CACHEKEY_ENV_LIST | `KUBE_CREDENTIAL_CACHE_USER,AWS_PROFILE,AWS_REGION,AWS_VAULT`                                                                                                                                                                                  | comma separated env names for additional cache-key |
| KUBE_CREDENTIAL_CACHE_DEBUG             | _unset_                                                                                                                                                                                                                                       | when set to a truthy value (`1`/`true`/`yes`/`on`), log cache key, hit/miss, expiry and refresh decisions to stderr |

#### Credentials without an expiry (default TTL)

Some credential plugins (for example the [passman](https://github.com/abenz1267/passman)
krew plugin) emit an `ExecCredential` with no `status.expirationTimestamp`. That
decodes to the zero time (`0001-01-01T00:00:00Z`), so the credential looks
permanently expired and would be re-fetched on **every** call — defeating the
cache while still leaving an entry in your secret store.

To handle this, when a refreshed credential's expiry is more than
`KUBE_CREDENTIAL_CACHE_NO_EXPIRY_THRESHOLD` (default `24h`) in the past, kcc-cache
treats it as "no expiry provided" and caches it for
`KUBE_CREDENTIAL_CACHE_DEFAULT_TTL` (default `1h`) instead. Genuinely
recently-expired credentials (within the threshold) still refresh as before.

#### Storage backends

By default credentials are kept **out of plaintext on disk** by storing them in the
operating system's secret store, and fall back to a plaintext file only when no
secret store is reachable (e.g. headless servers or CI).

- `keyring` — store credentials in the OS secret store. The store handles
  encryption-at-rest and ties access to your login session:
  - **macOS**: Keychain
  - **Linux**: Secret Service (GNOME Keyring / KWallet, via D-Bus)
  - **Windows**: Credential Manager
- `file` — store all credentials in a single plaintext JSON file
  (`KUBE_CREDENTIAL_CACHE_FILE`), protected only by filesystem permissions
  (`0600` file / `0700` directory).

Set `KUBE_CREDENTIAL_CACHE_BACKEND` explicitly to force a specific backend.

> :information_source: To clear cached credentials in the `keyring` backend,
> delete the `kube-credential-cache` entries from your OS secret store
> (Keychain Access on macOS, `secret-tool` / Seahorse on Linux, Credential
> Manager on Windows). For the `file` backend, remove the cache file.

### kcc-injector

```sh
$ kcc-injector -h
Usage: kcc-injector [flags] <kubeconfig filepath>
  -c string
        injection command (default "kcc-cache")
  -i    edit file in-place
  -r    restore kubeconfig to original
```
