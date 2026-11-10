package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

const (
	fileMinBytes   = 256 << 10      // smaller blocks stay inline: an upload round-trip costs more than resending them
	fileMaxUploads = 8              // per request, so a huge history cannot stall one turn
	fileExpiry     = 24 * time.Hour // expires_in_seconds sent with each upload
	fileCacheTTL   = 20 * time.Hour // shorter than fileExpiry so a cached id never outlives its file
	fileCacheMax   = 4096           // cached ids kept; expired ones go first, then the lot
	filesVersion   = "2026-10-21"   // anthropic-version of the GA Files API
	filesOffTTL    = 30 * time.Minute
)

// errFilesOAuth: api.anthropic.com answers /v1/files with 404 not_found_error for a subscription (OAuth) bearer.
var errFilesOAuth = errors.New(
	"files API not available with the subscription (OAuth) login; skipped until the credential changes",
)

// errFilesSkipped: a cached refusal skipped the upload; already logged when the refusal was recorded.
var errFilesSkipped = fmt.Errorf("%w (cached)", errFilesOAuth)

type fileEntry struct {
	id  string
	exp time.Time
}

// fileMedia maps a supported media type to the block type that may carry it.
var fileMedia = map[string]string{
	"image/jpeg": "image", "image/png": "image", "image/gif": "image", "image/webp": "image",
	"application/pdf": "document", "text/plain": "document",
}

// fileRewriter swaps large base64 blocks for uploaded file ids within one request.
type fileRewriter struct {
	r       *Router
	in      *http.Request
	p       *providers.Provider
	route   string
	scope   string // cache scope: file ids belong to one workspace/credential
	uploads int
	failed  bool
}

// fileScope names the account a file id belongs to: the provider plus a hash of the credential sent upstream.
func fileScope(in *http.Request, p *providers.Provider) (string, bool) {
	cred, err := providers.Credential(in.Context(), p)
	if errors.Is(err, providers.ErrPassthrough) { // cli auth forwards the client's own login
		cred, err = in.Header.Get("Authorization")+"\x00"+in.Header.Get("X-Api-Key"), nil
	}
	if err != nil {
		return "", false
	}
	return p.Name + "|" + hashHex([]byte(cred)), true
}

// rewriteFiles returns body with large base64 image/document blocks swapped for file sources; body is never mutated.
func (r *Router) rewriteFiles(in *http.Request, p *providers.Provider, route string, body []byte, count bool) []byte {
	if count || !anthropicOrigin(route) {
		return body
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	scope, ok := fileScope(in, p)
	if !ok {
		return body
	}
	fr := &fileRewriter{r: r, in: in, p: p, route: route, scope: scope}
	msgs, changed := fr.messages(m["messages"])
	if !changed {
		return body
	}
	m["messages"] = msgs
	out, err := marshalNoEscape(m)
	if err != nil {
		return body
	}
	return out
}

func (fr *fileRewriter) messages(raw json.RawMessage) (json.RawMessage, bool) {
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return raw, false
	}
	changed := false
	for i, msg := range msgs {
		var mm map[string]json.RawMessage
		if json.Unmarshal(msg, &mm) != nil {
			continue
		}
		content, ok := fr.blocks(mm["content"])
		if !ok {
			continue
		}
		mm["content"] = content
		if b, err := marshalNoEscape(mm); err == nil {
			msgs[i], changed = b, true
		}
	}
	if !changed {
		return raw, false
	}
	b, err := marshalNoEscape(msgs)
	return b, err == nil
}

// blocks rewrites a content array (top-level or a tool_result's); non-arrays are left alone.
func (fr *fileRewriter) blocks(raw json.RawMessage) (json.RawMessage, bool) {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return raw, false
	}
	changed := false
	for i, blk := range arr {
		if nb, ok := fr.block(blk); ok {
			arr[i], changed = nb, true
		}
	}
	if !changed {
		return raw, false
	}
	b, err := marshalNoEscape(arr)
	return b, err == nil
}

func (fr *fileRewriter) block(raw json.RawMessage) (json.RawMessage, bool) {
	var b map[string]json.RawMessage
	if json.Unmarshal(raw, &b) != nil {
		return raw, false
	}
	var typ string
	_ = json.Unmarshal(b["type"], &typ)
	switch typ {
	case "tool_result":
		content, ok := fr.blocks(b["content"])
		if !ok {
			return raw, false
		}
		b["content"] = content
	case "image", "document":
		src, ok := fr.source(typ, b["source"])
		if !ok {
			return raw, false
		}
		b["source"] = src
	default:
		return raw, false
	}
	out, err := marshalNoEscape(b)
	return out, err == nil
}

// source returns the file source for a large supported base64 source, else ok=false.
func (fr *fileRewriter) source(typ string, raw json.RawMessage) (json.RawMessage, bool) {
	var f struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if json.Unmarshal(raw, &f) != nil || f.Type != "base64" || fileMedia[f.MediaType] != typ ||
		len(f.Data)/4*3 < fileMinBytes {
		return raw, false
	}
	data, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil || len(data) < fileMinBytes {
		return raw, false
	}
	id, ok := fr.fileID(data, f.MediaType)
	if !ok {
		return raw, false
	}
	out, err := json.Marshal(map[string]string{"type": "file", "file_id": id})
	return out, err == nil
}

func (fr *fileRewriter) fileID(data []byte, mediaType string) (string, bool) {
	key := fr.scope + "|" + mediaType + "|" + hashHex(data)
	if id, ok := fr.r.cachedFile(key); ok {
		return id, true
	}
	if fr.failed || fr.uploads >= fileMaxUploads {
		return "", false
	}
	fr.uploads++
	id, err := fr.r.uploadFile(fr.in, fr.p, fr.route, data, mediaType)
	if err != nil {
		fr.failed = true
		if errors.Is(err, errFilesSkipped) {
			return "", false
		}
		fmt.Fprintf(fr.r.log, "proxy: files upload failed, sending inline: %v\n", err)
		return "", false
	}
	fr.r.storeFile(key, id)
	return id, true
}

func (r *Router) cachedFile(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.files[key]
	if !ok || !r.now().Before(e.exp) {
		delete(r.files, key)
		return "", false
	}
	return e.id, true
}

func (r *Router) storeFile(key, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.files == nil {
		r.files = map[string]fileEntry{}
	}
	now := r.now()
	if len(r.files) >= fileCacheMax {
		for k, e := range r.files {
			if !now.Before(e.exp) {
				delete(r.files, k)
			}
		}
		if len(r.files) >= fileCacheMax { // a dropped id only costs a re-upload
			clear(r.files)
		}
	}
	r.files[key] = fileEntry{id: id, exp: now.Add(fileCacheTTL)}
}

// uploadFile POSTs data to <route>/v1/files and returns the new file id.
func (r *Router) uploadFile(in *http.Request, p *providers.Provider, route string, data []byte,
	mediaType string) (string, error) {
	form, contentType, err := fileForm(data, mediaType)
	if err != nil {
		return "", err
	}
	login := http.Header{}
	if p.Auth.Method() == providers.AuthCLI {
		r.passthroughAuth(login, in, route)
	}
	bearer := login.Get("Authorization")
	if r.filesRefused(bearer) {
		return "", errFilesSkipped
	}
	quiet := in.WithContext(
		context.WithValue(in.Context(), attemptsKey{}, new(int)),
	) // uploads are not routing attempts
	resp, err := r.send(quiet, p, route+"/v1/files", form, func(h http.Header) {
		h.Set("Content-Type", contentType)
		h.Set("Anthropic-Version", filesVersion)
		maps.Copy(h, login)
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if bearer != "" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden) {
		r.refuseFiles(bearer)
		return "", fmt.Errorf("status %d: %w", resp.StatusCode, errFilesOAuth)
	}
	var out struct{ ID string }
	if resp.StatusCode/100 != 2 || json.Unmarshal(b, &out) != nil || out.ID == "" {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return out.ID, nil
}

// filesRefused reports whether the Files API refused this bearer within filesOffTTL.
func (r *Router) filesRefused(bearer string) bool {
	if bearer == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.filesOff == hashHex([]byte(bearer)) && r.now().Before(r.filesTill)
}

func (r *Router) refuseFiles(bearer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.filesOff, r.filesTill = hashHex([]byte(bearer)), r.now().Add(filesOffTTL)
}

// fileForm builds the multipart body for /v1/files and returns it with its Content-Type.
func fileForm(data []byte, mediaType string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="upload"`},
		"Content-Type":        {mediaType},
	})
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	if err := mw.WriteField("expires_in_seconds", strconv.Itoa(int(fileExpiry/time.Second))); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
