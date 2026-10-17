package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	bough "github.com/scholzdev/boughline/native"
	"github.com/scholzdev/boughline/native/virtual"
)

type prefixes []string

func (p *prefixes) String() string { return strings.Join(*p, ",") }
func (p *prefixes) Set(s string) error {
	if err := bough.ValidPath(s, true); err != nil {
		return err
	}
	*p = append(*p, s)
	return nil
}
func env(name, fallback string) string {
	if s := os.Getenv(name); s != "" {
		return s
	}
	return fallback
}
func fatal(err error) {
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if f, ok := err.(*bough.Fault); ok && f.Details != nil {
			b, _ := json.MarshalIndent(f.Details, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		}
		os.Exit(1)
	}
}
func output(v any) { data, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(data)) }
func decodeFile(name string, v any) error {
	raw, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	return bough.Decode(raw, v)
}
func keys() error {
	seed, _ := hex.DecodeString(bough.RequestID() + bough.RequestID()[:16])
	private := ed25519.NewKeyFromSeed(seed)
	values := map[string]string{"BOUGH_SIGNING_SEED": hex.EncodeToString(seed), "BOUGH_PUBLIC_KEY": hex.EncodeToString(private.Public().(ed25519.PublicKey)), "BOUGH_ADMIN_TOKEN": bough.RequestID() + bough.RequestID(), "BOUGH_CACHE_KEY": bough.RequestID(), "BOUGH_DAEMON_KEY": bough.RequestID(), "BOUGH_FORGEJO_SECRET": bough.RequestID()}
	for _, name := range []string{"BOUGH_SIGNING_SEED", "BOUGH_PUBLIC_KEY", "BOUGH_ADMIN_TOKEN", "BOUGH_CACHE_KEY", "BOUGH_DAEMON_KEY", "BOUGH_FORGEJO_SECRET"} {
		fmt.Printf("%s=%s\n", name, values[name])
	}
	return nil
}
func objectStore() (*bough.S3Store, error) {
	endpoint := os.Getenv("BOUGH_S3_ENDPOINT")
	secure := env("BOUGH_S3_TLS", "true") != "false"
	scheme := "https://"
	if !secure {
		scheme = "http://"
	}
	if err := bough.ValidateOrigin(scheme + endpoint); err != nil {
		return nil, err
	}
	store, err := bough.OpenS3(endpoint, os.Getenv("BOUGH_S3_BUCKET"), os.Getenv("BOUGH_S3_ACCESS_KEY"), os.Getenv("BOUGH_S3_SECRET_KEY"), secure)
	if err != nil {
		return nil, err
	}
	return store, nil
}
func serve(ctx context.Context, role string, args []string) error {
	f := flag.NewFlagSet(role, flag.ContinueOnError)
	defaultListen := "127.0.0.1:8712"
	if role == "cache" {
		defaultListen = "127.0.0.1:8713"
	}
	listen := f.String("listen", defaultListen, "listen address")
	socket := f.String("socket", env("BOUGH_DAEMON_SOCKET", "/run/boughline/daemon.sock"), "daemon Unix socket")
	cache := f.String("cache", "var/objects", "disposable cache directory")
	diskBytes := f.Int64("disk-bytes", 20<<30, "disk cache limit")
	memoryBytes := f.Int64("memory-bytes", 256<<20, "memory cache limit")
	parent := f.String("parent", os.Getenv("BOUGH_PARENT_URL"), "single upstream tier HTTPS origin")
	warm := f.String("warm-snapshot", "", "periodically refreshed snapshot file")
	cert := f.String("tls-cert", "", "TLS certificate PEM")
	key := f.String("tls-key", "", "TLS key PEM")
	if err := f.Parse(args); err != nil {
		return err
	}
	if role != "daemon" && *cert == "" {
		host, _, err := net.SplitHostPort(*listen)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("non-loopback listeners require --tls-cert and --tls-key")
		}
	}
	var handler http.Handler
	if role == "server" {
		database, err := bough.OpenSQL(ctx, os.Getenv("BOUGH_SQL_URL"))
		if err != nil {
			return err
		}
		defer database.Pool.Close()
		store, err := objectStore()
		if err != nil {
			return err
		}
		seed, err := hex.DecodeString(os.Getenv("BOUGH_SIGNING_SEED"))
		if err != nil || len(seed) != ed25519.SeedSize {
			return fmt.Errorf("BOUGH_SIGNING_SEED must be a 32-byte hex seed")
		}
		api, err := bough.NewAPI(bough.NewEngine(database, bough.NewCachedObjects(store, *memoryBytes)), ed25519.NewKeyFromSeed(seed), os.Getenv("BOUGH_ADMIN_TOKEN"), env("BOUGH_OWNER", "scholzdev"), os.Getenv("BOUGH_CACHE_KEY"))
		if err != nil {
			return err
		}
		api.ForgeSecret = os.Getenv("BOUGH_FORGEJO_SECRET")
		handler = api
	} else {
		if role != "cache" && role != "daemon" {
			return fmt.Errorf("invalid tier role")
		}
		if err := bough.ValidateOrigin(*parent); err != nil {
			return err
		}
		public, err := hex.DecodeString(os.Getenv("BOUGH_PUBLIC_KEY"))
		if err != nil || len(public) != ed25519.PublicKeySize {
			return fmt.Errorf("BOUGH_PUBLIC_KEY must be a 32-byte hex public key")
		}
		upstreamKey, expected, incoming := os.Getenv("BOUGH_CACHE_KEY"), "server", os.Getenv("BOUGH_DAEMON_KEY")
		if role == "daemon" {
			upstreamKey, expected, incoming = os.Getenv("BOUGH_DAEMON_KEY"), "cache", ""
		}
		if len(upstreamKey) < 32 || role == "cache" && len(incoming) < 32 {
			return fmt.Errorf("tier secrets are missing or too short")
		}
		client, err := bough.SecureHTTP(os.Getenv("BOUGH_CA_FILE"))
		if err != nil {
			return err
		}
		disk := &bough.DiskObjects{Directory: *cache, Limit: *diskBytes}
		metrics := &bough.TierMetrics{}
		upstream := &bough.UpstreamObjects{Local: disk, URL: strings.TrimRight(*parent, "/"), Secret: upstreamKey, ExpectedRole: expected, HTTP: client, Metrics: metrics}
		service := bough.NewObjectService(role, bough.NewCachedObjects(upstream, *memoryBytes), ed25519.PublicKey(public), incoming)
		service.Metrics = metrics
		mux := http.NewServeMux()
		service.Register(mux)
		handler = mux
		if *warm != "" {
			if err = bough.WarmSnapshot(ctx, disk, *warm); err != nil {
				return err
			}
			go func() {
				last := time.Time{}
				ticker := time.NewTicker(time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						info, e := os.Stat(*warm)
						if e == nil && info.ModTime().After(last) {
							if e = bough.WarmSnapshot(ctx, disk, *warm); e != nil {
								log.Printf("snapshot refresh failed: %v", e)
							} else {
								last = info.ModTime()
							}
						}
					}
				}
			}()
		}
	}
	var listener net.Listener
	var err error
	if role == "daemon" {
		if err = os.MkdirAll(filepath.Dir(*socket), 0700); err != nil {
			return err
		}
		if _, err = os.Lstat(*socket); err == nil {
			return fmt.Errorf("daemon socket already exists; stop its owner or remove a verified stale socket")
		}
		listener, err = net.Listen("unix", *socket)
		if err == nil {
			err = os.Chmod(*socket, 0660)
		}
		defer os.Remove(*socket)
	} else {
		listener, err = net.Listen("tcp", *listen)
	}
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 120 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	go func() {
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(stop)
	}()
	fmt.Printf("Boughline %s listening on %s\n", role, listener.Addr())
	if *cert != "" || *key != "" {
		if *cert == "" || *key == "" {
			return fmt.Errorf("both TLS certificate and key are required")
		}
		err = server.ServeTLS(listener, *cert, *key)
	} else {
		err = server.Serve(listener)
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
func clientCommand(ctx context.Context, command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	server := f.String("server", env("BOUGH_SERVER", "http://127.0.0.1:8712"), "native API HTTPS origin")
	forgejo := f.String("forgejo", "", "Forgejo HTTPS origin; --owner/--repo identify the native project")
	repo := f.String("repo", os.Getenv("BOUGH_REPO"), "repository ID")
	socket := f.String("socket", env("BOUGH_DAEMON_SOCKET", "/run/boughline/daemon.sock"), "node daemon socket")
	rev := f.String("revision", "main", "revision or ref")
	ref := f.String("ref", "main", "landing ref")
	base := f.String("base", "", "diff base / merge left")
	onto := f.String("onto", "main", "rebase destination")
	message := f.String("m", "", "commit message")
	mergeParent := f.String("merge-parent", "", "second parent after manual resolution")
	source := f.String("source", "", "source Git repository (read only)")
	checkpoint := f.String("checkpoint", "import-checkpoint.json", "durable Git import checkpoint")
	gitRef := f.String("git-ref", "refs/heads/main", "source Git ref to map to main")
	file := f.String("file", "", "JSON request file")
	out := f.String("out", "", "snapshot destination")
	cache := f.String("cache", ".bough-cache", "bounded client cache")
	approve := f.Bool("approve", true, "approve or retract approval")
	owner := f.String("owner", env("BOUGH_OWNER", "scholzdev"), "repository owner")
	var sparse prefixes
	f.Var(&sparse, "sparse", "file/directory prefix; repeatable")
	if err := f.Parse(args); err != nil {
		return err
	}
	token := os.Getenv("BOUGH_TOKEN")
	ca := os.Getenv("BOUGH_CA_FILE")
	var workspace *bough.Workspace
	if command == "watch" || command == "status" || command == "add" || command == "commit" || command == "update" || command == "recover" || command == "land" && *rev == "main" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		workspace, err = bough.OpenWorkspace(cwd, token, ca)
		if err != nil {
			return err
		}
		unlock, err := workspace.Lock(command == "recover")
		if err != nil {
			return err
		}
		defer func() { unlock() }()
		switch command {
		case "watch":
			unlock()
			unlock = func() {}
			return workspace.Watch(ctx)
		case "status":
			status, err := workspace.Status()
			if err != nil {
				return err
			}
			names := []string{}
			for p := range status {
				names = append(names, p)
			}
			sort.Strings(names)
			for _, p := range names {
				fmt.Printf("%s %s\n", status[p], p)
			}
			fmt.Println("base", workspace.State.Revision)
			return nil
		case "add":
			return workspace.Add(f.Args())
		case "commit":
			id, err := workspace.Commit(ctx, *message, *mergeParent)
			if err == nil {
				fmt.Println(id)
			}
			return err
		case "update":
			id, err := workspace.Update(ctx, *rev)
			if err == nil {
				fmt.Println(id)
			}
			return err
		case "recover":
			id, err := workspace.Recover()
			if err == nil {
				fmt.Println(id)
			}
			return err
		case "land":
			if workspace.State.Pending != nil {
				return fmt.Errorf("resume pending commit first")
			}
			id, err := workspace.Client.Land(ctx, workspace.State.Revision, *ref)
			if err == nil {
				fmt.Println(id)
			}
			return err
		}
	}
	if *forgejo != "" {
		if err := bough.ValidateOrigin(*forgejo); err != nil {
			return err
		}
		h, err := bough.SecureHTTP(ca)
		if err != nil {
			return err
		}
		defer h.CloseIdleConnections()
		endpoint := strings.TrimRight(*forgejo, "/") + "/api/v1/repos/" + url.PathEscape(*owner) + "/" + url.PathEscape(*repo) + "/bough"
		request, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := h.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return fmt.Errorf("Forgejo native connection failed: HTTP %d", response.StatusCode)
		}
		var info struct {
			Server    string `json:"server"`
			Repo      string `json:"repo"`
			Transport string `json:"transport"`
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 16384))
		if err != nil {
			return err
		}
		if err = bough.Decode(raw, &info); err != nil {
			return err
		}
		// Never forward a PAT to a different origin supplied in a response.
		origin, _ := url.Parse(*forgejo)
		gateway, err := url.Parse(info.Server)
		if err != nil || gateway.Scheme != origin.Scheme || gateway.Host != origin.Host {
			return fmt.Errorf("Forgejo gateway changed origin")
		}
		*server, *repo = info.Server, info.Repo
	}
	c, err := bough.NewClient(*server, *repo, token, *socket, *cache, ca)
	if err != nil {
		return err
	}
	var result any
	switch command {
	case "import-git":
		if *source == "" {
			return fmt.Errorf("--source Git repository is required")
		}
		result, err = bough.ImportGit(ctx, c, *source, *checkpoint, *gitRef)
	case "repo-create":
		err = c.Request(ctx, "POST", "/v1/repos", bough.RepoConfig{ID: *repo, Owner: *owner}, &result)
	case "info":
		err = c.Request(ctx, "GET", "/v1/repos/"+*repo+"/info", nil, &result)
	case "clone":
		if f.NArg() != 1 {
			return fmt.Errorf("usage: bough clone --repo ID [--sparse PATH] DESTINATION")
		}
		workspace, err = bough.Clone(ctx, c, f.Arg(0), *rev, sparse)
		if err == nil {
			fmt.Println(workspace.Root)
			return nil
		}
	case "land":
		id, err := c.Resolve(ctx, *rev)
		if err != nil {
			return err
		}
		id, err = c.Land(ctx, id, *ref)
		if err == nil {
			fmt.Println(id)
		}
		return err
	case "show":
		id, err := c.Resolve(ctx, *rev)
		if err != nil {
			return err
		}
		err = c.Request(ctx, "GET", "/v1/repos/"+*repo+"/changes/"+id, nil, &result)
	case "diff":
		left, err := c.Resolve(ctx, *base)
		if err != nil {
			return err
		}
		right, err := c.Resolve(ctx, *rev)
		if err != nil {
			return err
		}
		err = c.Request(ctx, "POST", "/v1/repos/"+*repo+"/diff", map[string]string{"base": left, "target": right}, &result)
	case "merge":
		left, err := c.Resolve(ctx, *base)
		if err != nil {
			return err
		}
		right, err := c.Resolve(ctx, *rev)
		if err != nil {
			return err
		}
		err = c.Request(ctx, "POST", "/v1/repos/"+*repo+"/merge", map[string]string{"left": left, "right": right, "message": *message, "request_id": bough.RequestID()}, &result)
	case "rebase":
		id, err := c.Resolve(ctx, *rev)
		if err != nil {
			return err
		}
		target, err := c.Resolve(ctx, *onto)
		if err != nil {
			return err
		}
		err = c.Request(ctx, "POST", "/v1/repos/"+*repo+"/rebase", map[string]string{"revision": id, "onto": target, "request_id": bough.RequestID()}, &result)
	case "history", "blame", "copies", "search":
		if f.NArg() != 1 {
			return fmt.Errorf("file path required")
		}
		query := url.Values{"revision": []string{*rev}, "path": []string{f.Arg(0)}}
		if command == "search" {
			query.Set("q", f.Arg(0))
		}
		err = c.Request(ctx, "GET", "/v1/repos/"+*repo+"/"+command+"?"+query.Encode(), nil, &result)
	case "review":
		id, err := c.Resolve(ctx, *rev)
		if err != nil {
			return err
		}
		err = c.Request(ctx, "POST", "/v1/repos/"+*repo+"/review", map[string]any{"revision": id, "approve": *approve}, &result)
	case "commit-api", "token-create", "grant", "policy":
		if *file == "" {
			return fmt.Errorf("--file JSON is required")
		}
		var body any
		if err = decodeFile(*file, &body); err != nil {
			return err
		}
		resource := map[string]string{"commit-api": "commits", "token-create": "tokens", "grant": "principals", "policy": "policies"}[command]
		err = c.Request(ctx, "POST", "/v1/repos/"+*repo+"/"+resource, body, &result)
	case "snapshot-create":
		if *out == "" {
			return fmt.Errorf("--out is required")
		}
		return bough.CreateSnapshot(ctx, c, *rev, sparse, *out)
	case "mount":
		if f.NArg() != 1 {
			return fmt.Errorf("mountpoint required")
		}
		return virtual.Mount(ctx, c, *rev, sparse, f.Arg(0), filepath.Join(*cache, "hydrated"))
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err == nil {
		output(result)
	}
	return err
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Println(`bough — native Boughline source control

Workspace: clone, status, watch, add, commit, land, update, recover
Remote:    info, show, diff, merge, rebase, history, blame, search, copies, review
Import:    import-git
Access:    token-create, grant, policy, commit-api
Snapshots: snapshot-create, mount (Linux read-only FUSE)
Services:  server, cache, daemon
Admin:     keygen, repo-create, migrate, init-store, derive, backup, restore, verify

Use bough COMMAND --help for flags. Set BOUGH_TOKEN for client operations.`)
		return
	}
	command := os.Args[1]
	var err error
	switch command {
	case "server", "cache", "daemon":
		err = serve(ctx, command, os.Args[2:])
	case "derive":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		repo := f.String("repo", os.Getenv("BOUGH_REPO"), "native repository ID")
		kind := f.String("kind", "all", "derived index kind or all")
		if err = f.Parse(os.Args[2:]); err != nil {
			break
		}
		var db *bough.SQLMetadata
		db, err = bough.OpenSQL(ctx, os.Getenv("BOUGH_SQL_URL"))
		if err != nil {
			break
		}
		defer db.Pool.Close()
		var store *bough.S3Store
		store, err = objectStore()
		if err != nil {
			break
		}
		var processed map[string]int
		processed, err = bough.DeriveIndices(ctx, db, store, *repo, *kind)
		if err == nil {
			output(processed)
		}
	case "backup", "restore", "verify":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		file := f.String("file", "", "independent backup archive path")
		if err = f.Parse(os.Args[2:]); err != nil {
			break
		}
		if *file == "" && command != "verify" {
			err = fmt.Errorf("--file is required")
			break
		}
		var db *bough.SQLMetadata
		db, err = bough.OpenSQL(ctx, os.Getenv("BOUGH_SQL_URL"))
		if err != nil {
			break
		}
		defer db.Pool.Close()
		var store *bough.S3Store
		store, err = objectStore()
		if err != nil {
			break
		}
		if command == "verify" {
			var count int
			count, err = bough.Verify(ctx, db, store)
			if err == nil {
				output(map[string]int{"verified_objects": count})
			}
		} else if command == "backup" {
			err = bough.Backup(ctx, db, store, *file)
		} else {
			err = bough.Restore(ctx, db, store, *file)
		}
	case "keygen":
		err = keys()
	case "migrate":
		var database *bough.SQLMetadata
		database, err = bough.OpenSQL(ctx, os.Getenv("BOUGH_SQL_URL"))
		if err == nil {
			defer database.Pool.Close()
			err = database.Migrate(ctx)
		}
	case "init-store":
		var store *bough.S3Store
		store, err = objectStore()
		if err == nil {
			var exists bool
			exists, err = store.Client.BucketExists(ctx, store.Bucket)
			if err == nil && !exists {
				err = store.Client.MakeBucket(ctx, store.Bucket, minio.MakeBucketOptions{})
				code := minio.ToErrorResponse(err).Code
				if code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists" {
					err = nil
				}
			}
		}
	default:
		err = clientCommand(ctx, command, os.Args[2:])
	}
	fatal(err)
}
