package memory

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

const recallMinInjections = 5

type recallTally struct {
	Injected int `json:"injected"`
	Used     int `json:"used"`
}

type recallLearning struct {
	Offsets map[string]int64                     `json:"offsets"`
	Days    map[string]map[string]recallTally    `json:"days"`
	Pending map[string]map[string]map[string]int `json:"pending"`
}

func recallLearningPath() string {
	return filepath.Join(paths.MemoryStateDir(), "recall-learning.json")
}

var learnedBoosts paths.StampCache[map[string]float64]

func recallWeights() map[string]float64 {
	path := recallLearningPath()
	weights, _ := learnedBoosts.Get(path, func() (map[string]float64, error) {
		state, _, err := loadRecallLearning(path)
		if err != nil {
			return nil, err
		}
		return state.weights(), nil
	})
	return weights
}

func (s recallLearning) weights() map[string]float64 {
	totals := map[string]recallTally{}
	for _, entries := range s.Days {
		for id, t := range entries {
			sum := totals[id]
			totals[id] = recallTally{sum.Injected + t.Injected, sum.Used + t.Used}
		}
	}
	weights := make(map[string]float64, len(totals))
	for id, t := range totals {
		if t.Injected >= recallMinInjections {
			weights[id] = min(1.2, max(0.8, 0.8+0.4*float64(t.Used)/float64(t.Injected)))
		}
	}
	return weights
}

func loadRecallLearning(path string) (recallLearning, []byte, error) {
	s := recallLearning{map[string]int64{}, map[string]map[string]recallTally{}, map[string]map[string]map[string]int{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	return s, data, err
}

// LearnRecall checkpoints the access generations so a rename never counts a delivery twice.
func LearnRecall(now time.Time) error {
	log := AccessLogPath(accessDir())
	if _, err := os.Stat(filepath.Dir(log)); os.IsNotExist(err) {
		return nil
	}
	ran, err := paths.TryFileLock(log+".lock", accessLockWait, func() error {
		return learnRecallLocked(log, recallLearningPath(), now.UTC())
	})
	if !ran && err == nil {
		return paths.ErrLockTimeout
	}
	return err
}

func learnRecallLocked(log, path string, now time.Time) error {
	s, previous, err := loadRecallLearning(path)
	if err != nil {
		return err
	}
	start := now.AddDate(0, 0, 1-accessRotateDays).Format(time.DateOnly)
	s.expire(start)
	offsets := map[string]int64{}
	for i := accessGenerations; i >= 0; i-- {
		key, end, err := s.consume(accessGeneration(log, i), start)
		if err != nil {
			return err
		}
		if key != "" {
			offsets[key] = end
		}
	}
	s.Offsets = offsets
	if previous == nil && len(s.Days) == 0 {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil || bytes.Equal(data, previous) {
		return err
	}
	return paths.AtomicWrite(path, data, 0o600)
}

func (s *recallLearning) expire(start string) {
	for day := range s.Days {
		if day < start {
			delete(s.Days, day)
			delete(s.Pending, day)
		}
	}
}

func (s *recallLearning) consume(path, start string) (string, int64, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	first, err := r.ReadString('\n')
	if err == io.EOF {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	digest := sha256.Sum256([]byte(first))
	key := hex.EncodeToString(digest[:])
	offset := s.Offsets[key]
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", 0, err
	}
	r.Reset(f)
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF {
			return key, offset, nil
		}
		if err != nil {
			return "", 0, err
		}
		s.observe(strings.TrimSuffix(line, "\n"), start)
		offset += int64(len(line))
	}
}

func (s *recallLearning) observe(line, start string) {
	ts, id, reason, session := parseAccessLine(line)
	if ts.IsZero() || !looksLikeSessionID(session) || id == "" {
		return
	}
	day := ts.UTC().Format(time.DateOnly)
	if day < start {
		return
	}
	switch reason {
	case accessRecall:
		s.inject(day, id, session)
	case AccessShow, AccessSearch:
		for day, sessions := range s.Pending {
			if ids := sessions[session]; ids[id] > 0 {
				t := s.Days[day][id]
				t.Used += ids[id]
				s.Days[day][id] = t
				delete(ids, id)
				if len(ids) == 0 {
					delete(sessions, session)
				}
			}
		}
	}
}

func (s *recallLearning) inject(day, id, session string) {
	if s.Days[day] == nil {
		s.Days[day] = map[string]recallTally{}
		s.Pending[day] = map[string]map[string]int{}
	}
	t := s.Days[day][id]
	t.Injected++
	s.Days[day][id] = t
	if s.Pending[day][session] == nil {
		s.Pending[day][session] = map[string]int{}
	}
	s.Pending[day][session][id]++
}

func learnRecallForIndex(errw io.Writer) {
	if err := LearnRecall(time.Now()); err != nil {
		fmt.Fprintf(errw, "memory index: recall learning: %v\n", err)
	}
}
