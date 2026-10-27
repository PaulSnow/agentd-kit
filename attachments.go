package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Attachments (#61): the owner attaches screenshots, PDFs and text files
// to a console message, pasted, dropped or picked on the page. Each file
// is checked by what its bytes are, never by what the browser calls it,
// stored once under the state directory (the event log's) in
// attachments/REPO/, named by its SHA-256, and recorded as an attachment
// event of the message it came with, in the same write. An agent is given
// each by name and path, never its bytes: an image costs tokens only when
// an agent opens it.

// Bounds of what one message carries.
const (
	maxAttachment  = 20 << 20 // bytes of one file
	maxAttachments = 10       // files of one message
	attachmentName = 120      // characters of a file's name, as kept
)

// attachmentExts are the types agentd takes, as their bytes read, and the
// extension each is stored with, by which an agent's Read knows an image
// or a PDF.
var attachmentExts = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp",
	"application/pdf": ".pdf", "text/plain": ".txt",
}

// attachmentTypesText names the allowed types, for a refusal.
const attachmentTypesText = "PNG, JPEG, GIF, WebP, PDF or plain text"

// AttachmentPayload is one file of a message: its id, the instruction it
// came with, the repository, or console without one, whose directory
// holds it, its name as the owner gave it, screened, its type as its
// bytes read, its size and its SHA-256.
type AttachmentPayload struct {
	ID         string `json:"id"`
	Message    string `json:"message"`
	Repository string `json:"repository"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Bytes      int64  `json:"bytes"`
	Hash       string `json:"hash"`
}

var attachmentHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// check says whether a is an attachment agentd could have recorded.
func (a *AttachmentPayload) check() error {
	switch {
	case a == nil:
		return errors.New("no attachment")
	case a.ID == "" || a.Message == "" || a.Repository == "" || a.Name == "":
		return errors.New("an attachment names its id, message, repository and name")
	case attachmentExts[a.Type] == "":
		return fmt.Errorf("type %q is not one agentd takes", a.Type)
	case a.Bytes <= 0 || a.Bytes > maxAttachment:
		return fmt.Errorf("%d bytes", a.Bytes)
	case !attachmentHash.MatchString(a.Hash):
		return errors.New("its hash is no SHA-256")
	}
	return nil
}

// AttachmentRef is an attachment as an agent is given it: a regular file
// at Path it can open with Read.
type AttachmentRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Bytes int64  `json:"bytes"`
	Path  string `json:"path"`
}

// safeDirName is a directory name for repo: the name itself when it is a
// plain one, else one made of its hash, so no name reaches outside
// attachments/.
var safeDirName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// attachmentsDir is the directory that holds repo's attachments: under the
// directory of cfg's event log, its state directory.
func attachmentsDir(cfg *Config, repo string) string {
	name := repo
	if !safeDirName.MatchString(name) || strings.Contains(name, "..") {
		name = "r-" + shortHash(HashPane(repo))
	}
	return filepath.Join(filepath.Dir(cfg.EventLog), "attachments", name)
}

// attachmentPath is where a is stored.
func attachmentPath(cfg *Config, a AttachmentPayload) string {
	return filepath.Join(attachmentsDir(cfg, a.Repository), a.Hash+attachmentExts[a.Type])
}

// stagedAttachment is a file a POST carries, checked, not stored yet.
type stagedAttachment struct {
	name string
	typ  string
	size int64
	file *multipart.FileHeader
}

// sniffType is the type b, a file's first bytes, reads as, when it is one
// agentd takes.
func sniffType(b []byte) (string, bool) {
	t, _, _ := strings.Cut(http.DetectContentType(b), ";")
	t = strings.TrimSpace(t)
	return t, attachmentExts[t] != ""
}

// checkAttachments checks the files a POST carries before any is stored:
// at most maxAttachments, none empty or over maxAttachment, and each a
// type agentd takes as its bytes read. The first that fails refuses all.
func checkAttachments(files []*multipart.FileHeader) ([]stagedAttachment, error) {
	if len(files) > maxAttachments {
		return nil, fmt.Errorf("a message carries at most %d files; this one has %d", maxAttachments, len(files))
	}
	var out []stagedAttachment
	for _, fh := range files {
		name := cleanAttachmentName(fh.Filename)
		if fh.Size <= 0 {
			return nil, fmt.Errorf("%s is empty", name)
		}
		if fh.Size > maxAttachment {
			return nil, fmt.Errorf("%s is %.1f MB, over the %d MB a file may be", name, float64(fh.Size)/(1<<20), maxAttachment>>20)
		}
		f, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("%s could not be read: %w", name, err)
		}
		head := make([]byte, 512)
		n, _ := io.ReadFull(f, head)
		f.Close()
		typ, ok := sniffType(head[:n])
		if !ok {
			return nil, fmt.Errorf("%s is %s; agentd takes %s files only", name, typ, attachmentTypesText)
		}
		out = append(out, stagedAttachment{name: name, typ: typ, size: fh.Size, file: fh})
	}
	return out, nil
}

// cleanAttachmentName is a file's name as kept: its base name, without
// control characters, at most attachmentName characters; "file" when
// nothing is left.
func cleanAttachmentName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		name = "file"
	}
	if r := []rune(name); len(r) > attachmentName {
		name = string(r[:attachmentName])
	}
	return name
}

// storeAttachments stores the staged files of message under repo's
// directory, each once by its SHA-256, and returns their records. A name
// that matches a confidential pattern is kept as its type's generic one.
// What it stored before a failure stays: a file named by its hash is
// never wrong, and a later message may name it.
func storeAttachments(cfg *Config, repo, message string, staged []stagedAttachment) ([]AttachmentPayload, error) {
	if len(staged) == 0 {
		return nil, nil
	}
	if cfg.EventLog == "" {
		return nil, errors.New("agentd has no state directory to keep files in: event_log is not set")
	}
	dir := attachmentsDir(cfg, repo)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	secret := confidentialMatcher(cfg)
	var out []AttachmentPayload
	for i, s := range staged {
		hash, size, err := storeOne(dir, s)
		if err != nil {
			return nil, fmt.Errorf("%s was not stored: %w", s.name, err)
		}
		name := s.name
		if secret(name) {
			name = "attachment" + attachmentExts[s.typ]
		}
		out = append(out, AttachmentPayload{ID: "Y" + shortHash(HashPane(message + "\n" + strconv.Itoa(i) + "\n" + hash))[:10],
			Message: message, Repository: repo, Name: name, Type: s.typ, Bytes: size, Hash: hash})
	}
	return out, nil
}

// storeOne copies s into dir through a temporary file, hashing it, and
// names it by its hash; a file stored before under that name is kept.
func storeOne(dir string, s stagedAttachment) (string, int64, error) {
	src, err := s.file.Open()
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	f, err := os.CreateTemp(dir, ".upload-*.tmp")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, maxAttachment+1))
	if err == nil && n != s.size {
		err = fmt.Errorf("read %d bytes of %d", n, s.size)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	dst := filepath.Join(dir, hash+attachmentExts[s.typ])
	if info, err := os.Stat(dst); err == nil && info.Mode().IsRegular() && info.Size() == n {
		return hash, n, nil
	}
	return hash, n, os.Rename(tmp, dst)
}

// attachmentRefs are atts as an agent is given them, at the paths path
// gives.
func attachmentRefs(atts []AttachmentPayload, path func(AttachmentPayload) string) []AttachmentRef {
	var out []AttachmentRef
	for _, a := range atts {
		out = append(out, AttachmentRef{ID: a.ID, Name: a.Name, Type: a.Type, Bytes: a.Bytes, Path: path(a)})
	}
	return out
}

// workdirAttachments is the directory of a work session's copies, in its
// workdir; its own .gitignore keeps them out of any commit.
const workdirAttachments = "attachments"

// copyAttachments copies atts into workdir's attachments/, each once by
// its stored name, and returns them as the session is given them: a work
// session reads in its workdir, never agentd's state directory.
func copyAttachments(cfg *Config, workdir string, atts []AttachmentPayload) ([]AttachmentRef, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	dir := filepath.Join(workdir, workdirAttachments)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); errors.Is(err, os.ErrNotExist) {
		if err := writeAtomic(filepath.Join(dir, ".gitignore"), []byte("*\n")); err != nil {
			return nil, err
		}
	}
	for _, a := range atts {
		dst := filepath.Join(dir, a.Hash+attachmentExts[a.Type])
		if info, err := os.Stat(dst); err == nil && info.Mode().IsRegular() && info.Size() == a.Bytes {
			continue
		}
		b, err := os.ReadFile(attachmentPath(cfg, a))
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", a.ID, err)
		}
		if err := writeAtomic(dst, b); err != nil {
			return nil, err
		}
	}
	return attachmentRefs(atts, func(a AttachmentPayload) string {
		return filepath.Join(dir, a.Hash+attachmentExts[a.Type])
	}), nil
}

// hasAttachment says whether atts holds the attachment id.
func hasAttachment(atts []AttachmentPayload, id string) bool {
	for _, a := range atts {
		if a.ID == id {
			return true
		}
	}
	return false
}
