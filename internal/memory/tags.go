package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Tag is one DECISION/BUG/NEXT/NOTE line the assistant wrote in a reply.
type Tag struct {
	Kind string
	Text string
}

// tagMaxLen caps a single tag; tagMaxCount keeps only the newest tags (dedup preserves first-seen order).
const (
	tagMaxLen   = 300
	tagMaxCount = 40
)

var tagKinds = []string{"DECISION:", "BUG:", "NEXT:", "NOTE:"}

// ExtractTags streams transcript JSONL and returns every DECISION/BUG/NEXT/NOTE line from ASSISTANT
// text blocks only — user, tool_result and system content never contribute a tag.
func ExtractTags(r io.Reader) ([]Tag, error) {
	var tags []Tag
	seen := map[string]bool{}
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			for _, t := range tagsFromLine(line) {
				key := t.Kind + "\x00" + t.Text
				if !seen[key] {
					seen[key] = true
					tags = append(tags, t)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	if len(tags) > tagMaxCount {
		tags = tags[len(tags)-tagMaxCount:]
	}
	return tags, nil
}

// tagsFromLine parses one JSONL record and extracts tags from its assistant text blocks, if any.
func tagsFromLine(line []byte) []Tag {
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil {
		return nil
	}
	if rec.Type != "assistant" || rec.Message.Role != "assistant" {
		return nil
	}
	var tags []Tag
	for _, block := range rec.Message.Content {
		if block.Type != "text" {
			continue
		}
		for ln := range strings.SplitSeq(block.Text, "\n") {
			if t, ok := parseTagLine(ln); ok {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

// parseTagLine matches an optional "- "/"* " bullet then a DECISION:/BUG:/NEXT:/NOTE: prefix.
func parseTagLine(ln string) (Tag, bool) {
	ln = strings.TrimSpace(ln)
	ln = strings.TrimPrefix(strings.TrimPrefix(ln, "- "), "* ")
	for _, kind := range tagKinds {
		if strings.HasPrefix(ln, kind) {
			text := strings.TrimSpace(strings.TrimPrefix(ln, kind))
			if len(text) > tagMaxLen {
				text = text[:tagMaxLen]
			}
			return Tag{Kind: strings.TrimSuffix(kind, ":"), Text: text}, true
		}
	}
	return Tag{}, false
}
