package imaged

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gokrazy/internal/gpt"
	"github.com/google/renameio/v2"
	"github.com/klauspost/pgzip"
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

	// getUsedBytes takes the path to an image and returns the number of bytes
	// that are actually used (by the MBR/GPT and rootA partition).
	getUsedBytes func(imagePath string) (int64, error)
}

func (s *server) index(w http.ResponseWriter, r *http.Request) error {
	http.Redirect(w, r, "https://github.com/gokrazy/imaged", http.StatusFound)
	return nil
}

func (s *server) validBase(base string) error {
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
	return nil
}

func (s *server) customize(w http.ResponseWriter, r *http.Request) error {
	// TODO: actually parse accept-encoding header
	// TODO: add support for zstd, add a benchmark to see in which order to negotiate
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		// TODO: behind a flag, allow uncompressed downloads (resulting in much
		// higher bandwidth usage)
		return httpError(http.StatusBadRequest, fmt.Errorf("your browser does not support transparent compression (Accept-Encoding: gzip)"))
	}

	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/customize/")
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return httpError(http.StatusNotFound, fmt.Errorf("syntax: /api/v1/customize/<base>/<image>/<file>"))
	}
	base, imageDir, filename := parts[0], parts[1], parts[2]
	if err := s.validBase(base); err != nil {
		return err
	}
	path := filepath.Join(s.ingestDir, base, imageDir, filename)
	// The filename should be requested without the .gz suffix, but the ingested
	// file is always stored as .gz file on disk.
	if !strings.HasSuffix(path, ".gz") {
		path += ".gz"
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	used, err := s.getUsedBytes(path)
	if err != nil {
		return err
	}

	zr, err := pgzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()

	wr := pgzip.NewWriter(w)
	defer wr.Close()
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	// Copy the start of the image as it is.
	start := &io.LimitedReader{R: zr, N: used}
	if _, err := io.Copy(wr, start); err != nil {
		return err
	}

	// Inject the extra configuration in the unused space.
	injected := strings.NewReader("HELLO WORLD")
	n, err := io.Copy(wr, injected)
	if err != nil {
		return err
	}
	// Skip the corresponding number of bytes in the original image.
	if _, err := io.Copy(ioutil.Discard, &io.LimitedReader{R: zr, N: n}); err != nil {
		return err
	}

	// Copy the rest of the image.
	if _, err := io.Copy(wr, zr); err != nil {
		return err
	}

	if err := wr.Close(); err != nil {
		return err
	}

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
	if err := s.validBase(base); err != nil {
		return err
	}

	dir, err := os.MkdirTemp(filepath.Join(s.ingestDir, base), "image-")
	if err != nil {
		return err
	}

	filename := "full.img.gz"
	f, err := renameio.NewPendingFile(filepath.Join(dir, filename))
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

	// TODO: can we rename full.img.gz to <base>_<build>.img by extracting the
	// build id from the image?

	b, err := json.Marshal(struct {
		CustomizeURL string
	}{
		CustomizeURL: "/api/v1/customize/" + base + "/" + filepath.Base(dir) + "/" + strings.TrimSuffix(filename, ".gz"),
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(b)
	return err
}

func getUsedBytesFromGPT(imagePath string) (int64, error) {
	f, err := os.Open(imagePath)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	zr, err := pgzip.NewReader(f)
	if err != nil {
		return -1, err
	}
	defer zr.Close()
	// TODO: fall back to BIOS in case no GPT is present (odroid hc2 images)
	parts, err := gpt.PartitionEntries(zr)
	if err != nil {
		return -1, err
	}
	// boot is parts[0]
	// rootA is parts[1]
	rootB := parts[2]
	byteOffset := int64(rootB.FirstLBA) * 512
	return byteOffset, nil
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
		ingestDir:    *ingestDir,
		getUsedBytes: getUsedBytesFromGPT,
	}

	mux := http.NewServeMux()
	mux.Handle("/", handleError(srv.index))
	mux.Handle("/api/v1/ingest/", handleError(srv.ingest))
	mux.Handle("/api/v1/customize/", handleError(srv.customize))

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
