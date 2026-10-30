package web

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/plans"
)

const (
	maxPlanBytes = 1 << 20 // /api/plan refuses to read a larger file
	rootRepo     = "claude-code"
)

type planEntry struct {
	Repo    string   `json:"repo"`
	Path    string   `json:"path"` // relative to the plans root, slash form
	Ticked  int      `json:"ticked"`
	Total   int      `json:"total"`
	Partial int      `json:"partial"`
	Claims  []string `json:"claims"`
	Mtime   int64    `json:"mtime"` // unix seconds
}

type planTask struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Done    bool     `json:"done"`
	Sha     string   `json:"sha"`
	Claim   string   `json:"claim"`
	Partial []string `json:"partial"`
	Owns    []string `json:"owns"`
}

type planDetail struct {
	Path     string     `json:"path"`
	Markdown string     `json:"markdown"`
	Tasks    []planTask `json:"tasks"`
}

func (s *Server) plansRoot() string { return filepath.Join(s.opts.ClaudeDir, "plans") }

// plansDirs is the plans root plus each direct subdirectory (one per repo).
func (s *Server) plansDirs() []string {
	root := s.plansRoot()
	dirs := []string{root}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs
}

// plansHub returns the plans hub, created on first use like treeHub.
func (s *Server) plansHub() *hub {
	s.plansOnce.Do(func() { s.plans = newHub(s.plansJSON, s.plansFingerprint, s.logger.Printf) })
	return s.plans
}

// plansJSON lists every plan with a checklist, newest first, marshalled like writeJSON (trailing newline).
func (s *Server) plansJSON() ([]byte, error) {
	sums, err := plans.Status(s.plansDirs()...)
	if err != nil {
		return nil, err
	}
	root := s.plansRoot()
	out := make([]planEntry, 0, len(sums))
	for _, sum := range sums {
		rel, err := filepath.Rel(root, sum.Path)
		if err != nil {
			continue
		}
		repo := rootRepo
		if d := filepath.Dir(rel); d != "." {
			repo = d
		}
		var mtime int64
		if info, err := os.Stat(sum.Path); err == nil {
			mtime = info.ModTime().Unix()
		}
		claims := sum.Claims
		if claims == nil {
			claims = []string{}
		}
		out = append(out, planEntry{
			Repo: repo, Path: filepath.ToSlash(rel), Ticked: sum.Ticked, Total: sum.Total,
			Partial: sum.Partial, Claims: claims, Mtime: mtime,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Mtime != out[j].Mtime {
			return out[i].Mtime > out[j].Mtime
		}
		return out[i].Path < out[j].Path
	})
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// plansFingerprint hashes name, mtime and size of every *.md in the plans dirs.
func (s *Server) plansFingerprint() uint64 {
	h := fnv.New64a()
	for _, dir := range s.plansDirs() {
		files, _ := filepath.Glob(filepath.Join(dir, "*.md"))
		for _, f := range files {
			info, err := os.Stat(f)
			if err != nil {
				continue
			}
			var b [16]byte
			binary.LittleEndian.PutUint64(b[:8], uint64(info.ModTime().UnixNano()))
			binary.LittleEndian.PutUint64(b[8:], uint64(info.Size()))
			_, _ = h.Write([]byte(f))
			_, _ = h.Write(b[:])
		}
	}
	return h.Sum64()
}

func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request) {
	b, err := s.plansJSON()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// resolvePlan maps a request path to a real .md file inside the plans root; false means reject.
func (s *Server) resolvePlan(rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") ||
		!strings.HasSuffix(rel, ".md") {
		return "", false
	}
	if slices.Contains(strings.Split(rel, "/"), "..") {
		return "", false
	}
	root, err := filepath.EvalSymlinks(s.plansRoot())
	if err != nil {
		return "", false
	}
	full, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	if in, err := filepath.Rel(root, full); err != nil || in == ".." || strings.HasPrefix(in, ".."+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	full, ok := s.resolvePlan(rel)
	if !ok {
		http.Error(w, "invalid plan path", http.StatusBadRequest)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "plan not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPlanBytes+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(b) > maxPlanBytes {
		http.Error(w, errors.New("plan exceeds 1 MiB").Error(), http.StatusRequestEntityTooLarge)
		return
	}
	tasks := make([]planTask, 0)
	for _, t := range plans.ParseBytes(full, b).Tasks {
		tasks = append(tasks, planTask{
			ID: t.ID, Title: t.Title, Done: t.Done, Sha: t.Sha, Claim: t.Claim,
			Partial: nonNil(t.Partial), Owns: nonNil(t.Owns),
		})
	}
	writeJSON(w, planDetail{Path: rel, Markdown: string(b), Tasks: tasks})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
