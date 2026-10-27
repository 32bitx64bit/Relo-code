package art

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"

	"github.com/relocode/relo/internal/sprite"
)

const (
	// maxPictureBytes bounds a picture file read from disk before it is decoded.
	maxPictureBytes = 40 << 20
	// maxRefEdge is the longest side of a stored reference picture.
	maxRefEdge = 1536
	// maxCandidateEdge is the longest side of a stored candidate; the image models' largest sizes are within it.
	maxCandidateEdge = 4096
	// maxStoredBytes bounds one stored PNG.
	maxStoredBytes = 24 << 20
	// MaxCandidates bounds the pictures one job keeps.
	MaxCandidates = 12
)

// stored is a picture as Relo stores it: PNG, no metadata, no larger than asked.
type stored struct {
	PNG    []byte
	Width  int
	Height int
	SHA256 string
}

// prepare decodes a picture, shrinks it to maxEdge if it is larger, and re-encodes it as a plain PNG. The re-encode
// drops whatever metadata the file carried, and keeps the alpha channel.
func prepare(data []byte, maxEdge int) (stored, error) {
	if len(data) > maxPictureBytes {
		return stored{}, fmt.Errorf("the picture is larger than %d MiB", maxPictureBytes>>20)
	}
	img, _, err := sprite.Decode(data)
	if err != nil {
		return stored{}, err
	}
	if img.Rect.Dx() > maxEdge || img.Rect.Dy() > maxEdge {
		frames, _, err := sprite.Run(img, []sprite.Op{{Op: "resize", MaxEdge: maxEdge, Filter: "area"}}, sprite.Env{})
		if err != nil {
			return stored{}, err
		}
		img = frames[0]
	}
	out, err := sprite.EncodePNG(img)
	if err != nil {
		return stored{}, err
	}
	if len(out) > maxStoredBytes {
		return stored{}, fmt.Errorf("the picture would be %d MiB as a PNG; the most stored is %d", len(out)>>20, maxStoredBytes>>20)
	}
	sum := sha256.Sum256(out)
	return stored{PNG: out, Width: img.Rect.Dx(), Height: img.Rect.Dy(), SHA256: hex.EncodeToString(sum[:])}, nil
}

// RefInfo describes a stored reference picture.
type RefInfo struct {
	ID     string `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int    `json:"bytes"`
}

// AddRef stores a reference picture and returns its ID. The same picture always gets the same ID, so adding it twice
// stores it once. The role and weight are chosen when a job uses the reference, not here.
func (s *Store) AddRef(data []byte) (RefInfo, error) {
	n, err := prepare(data, maxRefEdge)
	if err != nil {
		return RefInfo{}, err
	}
	info := RefInfo{ID: "r_" + n.SHA256[:12], Width: n.Width, Height: n.Height, Bytes: len(n.PNG)}
	path := filepath.Join(s.RefsDir(), info.ID+".png")
	if _, err := os.Stat(path); err == nil {
		return info, nil
	}
	if err := writeAtomic(path, n.PNG); err != nil {
		return RefInfo{}, err
	}
	return info, nil
}

// HasRef reports whether a stored reference exists.
func (s *Store) HasRef(id string) bool {
	if !strings.HasPrefix(id, "r_") || strings.ContainsAny(id, `/\.`) {
		return false
	}
	_, err := os.Stat(filepath.Join(s.RefsDir(), id+".png"))
	return err == nil
}

// RefInfoFor returns what is stored about a reference.
func (s *Store) RefInfoFor(id string) (RefInfo, bool) {
	if !s.HasRef(id) {
		return RefInfo{}, false
	}
	f, err := os.Open(filepath.Join(s.RefsDir(), id+".png"))
	if err != nil {
		return RefInfo{}, false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return RefInfo{}, false
	}
	st, _ := f.Stat()
	size := 0
	if st != nil {
		size = int(st.Size())
	}
	return RefInfo{ID: id, Width: cfg.Width, Height: cfg.Height, Bytes: size}, true
}

// attach stores pictures as the next candidates of a job and returns the ones added. A picture the job already has
// (same bytes once normalized) is skipped.
func (s *Store) attach(job Job, sources [][]byte) ([]Candidate, error) {
	have := map[string]bool{}
	for _, c := range job.Candidates {
		have[c.SHA256] = true
	}
	next := 1
	for _, c := range job.Candidates {
		if c.Index >= next {
			next = c.Index + 1
		}
	}
	var added []Candidate
	var firstErr error
	for _, data := range sources {
		if len(job.Candidates)+len(added) >= MaxCandidates {
			if firstErr == nil {
				firstErr = fmt.Errorf("a job keeps at most %d pictures", MaxCandidates)
			}
			break
		}
		n, err := prepare(data, maxCandidateEdge)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if have[n.SHA256] {
			continue
		}
		have[n.SHA256] = true
		name := fmt.Sprintf("%d.png", next)
		if err := os.MkdirAll(job.Dir, 0o755); err != nil {
			return added, err
		}
		if err := writeAtomic(filepath.Join(job.Dir, name), n.PNG); err != nil {
			return added, err
		}
		added = append(added, Candidate{Index: next, File: name, Width: n.Width, Height: n.Height, SHA256: n.SHA256})
		next++
	}
	if len(added) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return added, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readPicture reads a picture file from disk with a size limit.
func readPicture(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if st.Size() > maxPictureBytes {
		return nil, fmt.Errorf("larger than %d MiB", maxPictureBytes>>20)
	}
	buf := make([]byte, st.Size())
	if _, err := readFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func readFull(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			if total == len(buf) {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}
