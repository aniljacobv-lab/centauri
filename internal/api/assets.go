package api

// Local asset store: images and documents an AI can see. Uploaded bytes are
// stored as content-addressed blobs on disk (NOT in the JSONL log) and
// referenced by a small fact (asset:<sha>). PDFs are rendered to per-page
// images via an external rasteriser (poppler/ImageMagick) so go.mod stays
// dependency-free. A vision model then analyses each image-bearing fact via
// ENRICH (see internal/ceql/enrich.go). See docs/design-vision.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/proxima360/centauri/internal/model"
)

const maxAssetBytes = 64 << 20 // 64 MiB upload cap

func (s *Server) assetsDir() (string, error) {
	dir := s.dataDir()
	if dir == "" {
		return "", fmt.Errorf("asset storage needs a file-backed server (not the in-memory single DB)")
	}
	ad := filepath.Join(dir, "assets")
	if err := os.MkdirAll(ad, 0o755); err != nil {
		return "", err
	}
	return ad, nil
}

func extFor(mime, filename string) string {
	if e := filepath.Ext(filename); e != "" {
		return strings.ToLower(e)
	}
	switch {
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "jpeg"), strings.Contains(mime, "jpg"):
		return ".jpg"
	case strings.Contains(mime, "webp"):
		return ".webp"
	case strings.Contains(mime, "gif"):
		return ".gif"
	case strings.Contains(mime, "pdf"):
		return ".pdf"
	}
	return ".bin"
}

// handleAssetUpload stores an uploaded image/PDF as a content-addressed blob
// plus reference fact(s). Body = raw file bytes; Content-Type = mime;
// ?filename= names it. Returns the asset subject and (for PDFs) page count.
func (s *Server) handleAssetUpload(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	ad, err := s.assetsDir()
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxAssetBytes+1))
	if err != nil {
		httpErr(w, 400, "read body: "+err.Error())
		return
	}
	if len(data) == 0 {
		httpErr(w, 400, "empty upload")
		return
	}
	if len(data) > maxAssetBytes {
		httpErr(w, 413, "file too large (max 64 MiB)")
		return
	}

	mime := r.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	filename := r.URL.Query().Get("filename")
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	ext := extFor(mime, filename)
	blobPath := filepath.Join(ad, sha+ext)
	if _, statErr := os.Stat(blobPath); statErr != nil { // content-addressed: write once
		if err := os.WriteFile(blobPath, data, 0o644); err != nil {
			httpErr(w, 500, "write blob: "+err.Error())
			return
		}
	}

	now := time.Now().UnixMicro()
	resp := map[string]any{"sha256": sha, "mime": mime, "bytes": len(data), "filename": filename,
		"subject": "asset:" + sha}

	if strings.Contains(mime, "pdf") || strings.EqualFold(ext, ".pdf") {
		doc := &model.Event{Subject: "asset:" + sha, Facet: "doc", Type: model.Observed,
			Value: map[string]any{"kind": "pdf", "mime": mime, "filename": filename,
				"bytes": len(data), "sha256": sha, "path": blobPath},
			Provenance: model.SystemFeed, Confidence: 1, SourceSystem: "assets"}
		pages, note := renderPDF(blobPath, ad, sha)
		doc.Value["pages"] = len(pages)
		batch := []*model.Event{doc}
		for _, pp := range pages {
			batch = append(batch, &model.Event{
				Subject: fmt.Sprintf("asset:%s-p%d", sha, pp.n), Facet: "vision", Type: model.Observed,
				Value: map[string]any{"kind": "page", "page": pp.n, "parent": "asset:" + sha,
					"mime": "image/png", "image_path": pp.path, "filename": filename},
				Provenance: model.SystemFeed, Confidence: 1, SourceSystem: "assets"})
		}
		if err := st.Append(now, batch, nil); err != nil {
			httpErr(w, 422, err.Error())
			return
		}
		resp["kind"] = "pdf"
		resp["pages"] = len(pages)
		if note != "" {
			resp["render_note"] = note
		}
	} else {
		ev := &model.Event{Subject: "asset:" + sha, Facet: "vision", Type: model.Observed,
			Value: map[string]any{"kind": "image", "mime": mime, "filename": filename,
				"bytes": len(data), "sha256": sha, "image_path": blobPath},
			Provenance: model.SystemFeed, Confidence: 1, SourceSystem: "assets"}
		if err := st.Append(now, []*model.Event{ev}, nil); err != nil {
			httpErr(w, 422, err.Error())
			return
		}
		resp["kind"] = "image"
	}
	writeJSON(w, resp)
}

// handleAssetGet serves a stored blob by sha (or page sha like "<sha>-p1"),
// confined to the assets directory.
func (s *Server) handleAssetGet(w http.ResponseWriter, r *http.Request) {
	st := s.dbOr(w, r)
	if st == nil {
		return
	}
	ad, err := s.assetsDir()
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	sha := r.PathValue("sha")
	var path, mime string
	for _, e := range st.Current("asset:"+sha, "") {
		if p, ok := e.Value["image_path"].(string); ok && p != "" {
			path = p
		} else if p, ok := e.Value["path"].(string); ok && p != "" {
			path = p
		}
		if m, ok := e.Value["mime"].(string); ok {
			mime = m
		}
	}
	if path == "" {
		httpErr(w, 404, "no such asset")
		return
	}
	// Safety: never serve a path outside the assets directory.
	if rel, err := filepath.Rel(ad, filepath.Clean(path)); err != nil || strings.HasPrefix(rel, "..") {
		httpErr(w, 403, "asset path outside store")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		httpErr(w, 404, "asset blob missing on disk")
		return
	}
	defer f.Close()
	if mime != "" {
		w.Header().Set("Content-Type", mime)
	}
	_, _ = io.Copy(w, f)
}

type pdfPage struct {
	n    int
	path string
}

// renderPDF rasterises each PDF page to <ad>/<sha>-p-<k>.png using whichever
// external tool is installed (poppler first, then ImageMagick). Returns the
// pages (numbered positionally) and a human note if rendering was skipped or
// failed. Shelling out keeps go.mod free of any image/PDF dependency.
func renderPDF(pdfPath, ad, sha string) ([]pdfPage, string) {
	prefix := filepath.Join(ad, sha+"-p")
	run := func(tool string, args ...string) error {
		return exec.Command(tool, args...).Run()
	}
	switch {
	case lookExec("pdftoppm") != "":
		if err := run(lookExec("pdftoppm"), "-png", "-r", "150", pdfPath, prefix); err != nil {
			return nil, "pdftoppm failed: " + err.Error()
		}
	case lookExec("pdftocairo") != "":
		if err := run(lookExec("pdftocairo"), "-png", "-r", "150", pdfPath, prefix); err != nil {
			return nil, "pdftocairo failed: " + err.Error()
		}
	case lookExec("magick") != "":
		if err := run(lookExec("magick"), "-density", "150", pdfPath, prefix+"-%d.png"); err != nil {
			return nil, "magick failed: " + err.Error()
		}
	case lookExec("convert") != "":
		if err := run(lookExec("convert"), "-density", "150", pdfPath, prefix+"-%d.png"); err != nil {
			return nil, "convert failed: " + err.Error()
		}
	default:
		return nil, "no PDF rasteriser found — install poppler (pdftoppm) or ImageMagick to render pages; the PDF is stored and analysable once a renderer is available"
	}
	return collectPages(ad, sha), ""
}

func lookExec(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// collectPages gathers the rendered <sha>-p-*.png files and numbers them 1..k
// by their numeric suffix (positional, tool-independent).
func collectPages(ad, sha string) []pdfPage {
	entries, _ := os.ReadDir(ad)
	pre := sha + "-p-"
	type pf struct {
		num  int
		name string
	}
	var pfs []pf
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, pre) && strings.HasSuffix(name, ".png") {
			n, _ := strconv.Atoi(strings.TrimSuffix(name[len(pre):], ".png"))
			pfs = append(pfs, pf{n, name})
		}
	}
	sort.Slice(pfs, func(i, j int) bool { return pfs[i].num < pfs[j].num })
	pages := make([]pdfPage, 0, len(pfs))
	for i, p := range pfs {
		pages = append(pages, pdfPage{n: i + 1, path: filepath.Join(ad, p.name)})
	}
	return pages
}
