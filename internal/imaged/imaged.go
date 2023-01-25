package imaged

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/renameio/v2"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
)

type httpErr struct {
	code int
	err  error
}

func (h *httpErr) Error() string {
	return h.err.Error()
}

func httpError(code int, err error) error {
	return &httpErr{code, err}
}

func handleError(h func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		if err == context.Canceled {
			return // client canceled the request
		}
		code := http.StatusInternalServerError
		unwrapped := err
		if he, ok := err.(*httpErr); ok {
			code = he.code
			unwrapped = he.err
		}
		log.Printf("%s: HTTP %d %s", r.URL.Path, code, unwrapped)
		http.Error(w, unwrapped.Error(), code)
	})
}

func listenAndServe(ctx context.Context, srv *http.Server) error {
	errC := make(chan error)
	go func() {
		errC <- srv.ListenAndServe()
	}()
	select {
	case err := <-errC:
		return err
	case <-ctx.Done():
		timeout, canc := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer canc()
		_ = srv.Shutdown(timeout)
		return ctx.Err()
	}
}

type server struct {
	ingestDir string
}

func (s *server) index(w http.ResponseWriter, r *http.Request) error {
	// TODO: redirect to imaged repository README on GitHub
	return nil
}

// build:
//
//	% gok overwrite --full /tmp/full.img && pigz /tmp/full.img
//
// upload:
//
//	% curl -X PUT -H 'Content-Type: application/octet-stream' \
//	  --data-binary @/tmp/full.img.gz \
//	  http://localhost:1397/api/v1/ingest/scan2drive
func (s *server) ingest(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "PUT" {
		return httpError(http.StatusMethodNotAllowed, fmt.Errorf("invalid method"))
	}

	base := strings.TrimPrefix(r.URL.Path, "/api/v1/ingest/")
	// Instead of trying to strip base of path traversal attacks, we just list
	// the directory and only continue if we find a file that matches base.
	ents, err := os.ReadDir(s.ingestDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	found := slices.ContainsFunc(ents, func(ent os.DirEntry) bool {
		return ent.Name() == base
	})
	if !found {
		return httpError(http.StatusNotFound, fmt.Errorf("this directory is not configured for ingestion on this server yet"))
	}

	dir, err := os.MkdirTemp(filepath.Join(s.ingestDir, base), "image-")
	if err != nil {
		return err
	}

	f, err := renameio.NewPendingFile(filepath.Join(dir, "full.img.gz"))
	if err != nil {
		return err
	}
	defer f.Cleanup()

	if _, err := io.Copy(f, r.Body); err != nil {
		return err
	}

	// TODO: verify the file can be decompressed using gzip and discard it if not

	if err := f.CloseAtomicallyReplace(); err != nil {
		return err
	}

	return nil
}

func Main() error {
	var (
		listenAddrs = flag.String("listen_addrs",
			"localhost:1397",
			"(comma-separated list of) [host]:port HTTP listen addresses")

		ingestDir = flag.String("ingest_dir",
			"/var/lib/gokr-imaged/ingest",
			"path to the directory in which gokrazy images should be ingested into")
	)

	flag.Parse()

	srv := &server{
		ingestDir: *ingestDir,
	}

	mux := http.NewServeMux()
	mux.Handle("/", handleError(srv.index))
	mux.Handle("/api/v1/ingest/", handleError(srv.ingest))

	eg, ctx := errgroup.WithContext(context.Background())
	for _, addr := range strings.Split(*listenAddrs, ",") {
		srv := &http.Server{
			Handler: mux,
			Addr:    addr,
		}
		eg.Go(func() error {
			return listenAndServe(ctx, srv)
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	return nil
}
