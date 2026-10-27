package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testPNG is a small PNG, as a pasted screenshot is one.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// testPDF is the start of a PDF, as its bytes read.
var testPDF = []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")

// upload is one file of a multipart form.
type upload struct {
	name string
	body []byte
	pad  int64 // zero bytes after body
}

// multipartTo sends fields and files to path as the owner's page sends
// them, from the UI itself, with token.
func multipartTo(u *UI, path, token string, fields map[string]string, files ...upload) *httptest.ResponseRecorder {
	return multipartFrom(u, path, "http://"+pageAddr, token, fields, files...)
}

// multipartFrom is multipartTo with the Origin given. The body is
// streamed, so a large one is never held whole; the files the form
// spooled are removed, as http.Server removes them after a request.
func multipartFrom(u *UI, path, origin, token string, fields map[string]string, files ...upload) *httptest.ResponseRecorder {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		mw.WriteField("token", token)
		for k, v := range fields {
			mw.WriteField(k, v)
		}
		for _, f := range files {
			w, err := mw.CreateFormFile("file", f.name)
			if err == nil {
				_, err = w.Write(f.body)
			}
			if err == nil && f.pad > 0 {
				_, err = io.Copy(w, io.LimitReader(zeros{}, f.pad))
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.CloseWithError(mw.Close())
	}()
	req := httptest.NewRequest("POST", path, pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Host = pageAddr
	req.RemoteAddr = "192.0.2.20:51000"
	w := httptest.NewRecorder()
	u.ServeHTTP(w, req)
	pr.CloseWithError(io.ErrClosedPipe) // a refusal reads no further
	if req.MultipartForm != nil {
		req.MultipartForm.RemoveAll()
	}
	return w
}

// zeros reads as zero bytes without end.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// attachmentFixture is the UI fixture with its configs' state directory
// in a temporary one, which it returns.
func attachmentFixture(t *testing.T) (*UI, *uiFixture, string) {
	t.Helper()
	u, f := fixtureUI(t)
	dir := t.TempDir()
	f.coord.EventLog = filepath.Join(dir, "events.jsonl")
	f.night.EventLog = filepath.Join(dir, "events.jsonl")
	return u, f, dir
}

// coordState is the coord log folded.
func coordState(f *uiFixture) *LogState {
	st := NewLogState(f.coord, ModeLive)
	events, _, _ := NewLogWriter(f.coordBuf).Tail(0)
	st.Fold(events)
	return st
}

// A screenshot pasted into the composer is sent with the question as
// multipart: stored once by its SHA-256 under the state directory's
// attachments/widgets/, recorded as an attachment event of the message in
// the same write, shown as a thumbnail and served to the owner alone, as
// the type its bytes read as.
func TestUIAttachmentPasted(t *testing.T) {
	u, f, dir := attachmentFixture(t)
	shot := testPNG(t)
	w := multipartTo(u, "/respond", u.token, map[string]string{"src": "coord", "repository": "widgets", "text": "What is wrong here?", "back": "/console/widgets"},
		upload{"image.png", shot, 0}, upload{"notes.log", []byte("panic: squeak\n"), 0})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	before := f.coordBuf.String()
	var in *Instruction
	for _, i := range coordState(f).Instructions {
		if i.From == "ui" {
			in = i
		}
	}
	if in == nil || in.Text != "What is wrong here?" || len(in.Attachments) != 2 {
		t.Fatalf("instruction %+v", in)
	}
	a := in.Attachments[0]
	if a.Type != "image/png" || a.Name != "image.png" || a.Bytes != int64(len(shot)) || a.Message != in.ID || a.Repository != "widgets" || in.Attachments[1].Type != "text/plain" {
		t.Fatalf("attachments %+v", in.Attachments)
	}
	path := filepath.Join(dir, "attachments", "widgets", a.Hash+".png")
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, shot) || a.Hash != HashPane(string(shot)) {
		t.Fatalf("stored at %s: %v", path, err)
	}
	// The message and its files are one write: the instruction line is
	// followed at once by its attachments.
	lines := strings.Split(strings.TrimSpace(before), "\n")
	n := len(lines)
	if !strings.Contains(lines[n-3], `"kind":"instruction"`) || !strings.Contains(lines[n-2], `"kind":"attachment"`) || !strings.Contains(lines[n-1], `"kind":"attachment"`) {
		t.Fatalf("the last lines are not the message and its files:\n%s", strings.Join(lines[n-3:], "\n"))
	}
	// The same screenshot again is stored once; sent alone, it is a
	// message without words, not a withheld one.
	withheld := strings.Count(owner(t, u, "/console/widgets"), withheldMessage)
	if w := multipartTo(u, "/respond", u.token, map[string]string{"src": "coord", "repository": "widgets", "text": "", "back": "/console/widgets"}, upload{"image.png", shot, 0}); w.Code != http.StatusSeeOther {
		t.Fatalf("files alone: %d %s", w.Code, w.Body.String())
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "attachments", "widgets")); len(ents) != 2 {
		t.Fatalf("stored %d files, not 2", len(ents))
	}
	body := owner(t, u, "/console/widgets")
	url := "/attachment/coord/" + a.ID
	if !strings.Contains(body, `<img src="`+url+`" alt="image.png"`) || !strings.Contains(body, `notes.log`) || !strings.Contains(body, `TXT</span>`) {
		t.Fatalf("the page lacks the thumbnail or the file:\n%s", body)
	}
	if strings.Count(body, withheldMessage) != withheld {
		t.Fatal("files sent alone are shown as a withheld message")
	}
	got := get(u, "GET", url, pageAddr, "192.0.2.20:51000")
	if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), shot) || got.Header().Get("Content-Type") != "image/png" || got.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("serving: %d %v", got.Code, got.Header())
	}
	if !strings.Contains(got.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("CSP %q", got.Header().Get("Content-Security-Policy"))
	}
	for target, code := range map[string]int{url: http.StatusForbidden} {
		if w := get(u, "GET", target, pageAddr, "192.0.2.30:51000"); w.Code != code {
			t.Errorf("a guest: %d", w.Code)
		}
	}
	for _, target := range []string{"/attachment/coord/Ynone", "/attachment/elsewhere/" + a.ID, "/attachment/night/" + a.ID, "/attachment/coord"} {
		if w := get(u, "GET", target, pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
}

// A type agentd does not take, as its bytes read whatever its name says,
// a file over 20 MB, an empty one, eleven files, or a form without the
// UI's token is refused with a plain error, and nothing is stored or
// logged.
func TestUIAttachmentRefused(t *testing.T) {
	u, f, dir := attachmentFixture(t)
	shot := testPNG(t)
	fields := map[string]string{"src": "coord", "repository": "widgets", "text": "Look", "back": "/console/widgets"}
	eleven := make([]upload, 11)
	for i := range eleven {
		eleven[i] = upload{"shot.png", shot, 0}
	}
	cases := []struct {
		name  string
		token string
		files []upload
		code  int
		want  string
	}{
		{"html named png", u.token, []upload{{"x.png", []byte("<html><script>alert(1)</script></html>"), 0}}, http.StatusBadRequest, "x.png is text/html; agentd takes PNG, JPEG, GIF, WebP, PDF or plain text files only"},
		{"zip", u.token, []upload{{"a.zip", []byte("PK\x03\x04rest"), 0}}, http.StatusBadRequest, "agentd takes"},
		{"too big", u.token, []upload{{"big.png", shot, maxAttachment}}, http.StatusBadRequest, "over the 20 MB a file may be"},
		{"empty", u.token, []upload{{"none.txt", nil, 0}}, http.StatusBadRequest, "none.txt is empty"},
		{"eleven", u.token, eleven, http.StatusBadRequest, "at most 10 files"},
		{"a good one with a bad one", u.token, []upload{{"shot.png", shot, 0}, {"x.exe", []byte("MZ\x90\x00"), 0}}, http.StatusBadRequest, "x.exe is"},
		{"no token", "nope", []upload{{"shot.png", shot, 0}}, http.StatusForbidden, "token"},
	}
	for _, origin := range []string{"", "http://elsewhere.example"} {
		before := f.coordBuf.Len()
		if w := multipartFrom(u, "/respond", origin, u.token, fields, upload{"shot.png", shot, 0}); w.Code != http.StatusForbidden || f.coordBuf.Len() != before {
			t.Errorf("origin %q: %d", origin, w.Code)
		}
	}
	for _, c := range cases {
		before := f.coordBuf.Len()
		w := multipartTo(u, "/respond", c.token, fields, c.files...)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %q", c.name, w.Code, w.Body.String())
		}
		if f.coordBuf.Len() != before {
			t.Errorf("%s: logged", c.name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "attachments")); !os.IsNotExist(err) {
		t.Fatalf("something was stored: %v", err)
	}
	// The whole body is bounded too.
	huge := make([]upload, 11)
	for i := range huge {
		huge[i] = upload{"big.png", shot, maxAttachment}
	}
	if w := multipartTo(u, "/respond", u.token, fields, huge...); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "at most 10 files of at most 20 MB") {
		t.Fatalf("a body over the bound: %d %s", w.Code, w.Body.String())
	}
}

// A PDF attached to a response on a session's entry reaches the work
// session: copied into its workdir's attachments/, which its own
// .gitignore keeps out of commits, and named with its path in the
// instruction's record in instructions.json.
func TestAttachmentReachesWorkSession(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{EventLog: filepath.Join(dir, "state", "events.jsonl")}
	staged := stage(t, upload{"spec.pdf", testPDF, 0})
	atts, err := storeAttachments(cfg, "widgets", "I1", staged)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o755)
	in := &Instruction{InstructionPayload: InstructionPayload{ID: "I1", Text: "Read this", From: "ui", Re: &EntryRe{ID: "m:1", Kind: "info", Session: "w1"}}, Attachments: atts}
	if err := writeInstructions(cfg, work, []*Instruction{in}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Instructions []InstructionDoc `json:"instructions"`
	}
	b, _ := os.ReadFile(filepath.Join(work, InstructionsFile))
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Instructions) != 1 || len(doc.Instructions[0].Attachments) != 1 {
		t.Fatalf("instructions.json: %v\n%s", err, b)
	}
	ref := doc.Instructions[0].Attachments[0]
	if ref.Name != "spec.pdf" || ref.Type != "application/pdf" || ref.Path != filepath.Join(work, "attachments", atts[0].Hash+".pdf") || ref.ID != atts[0].ID {
		t.Fatalf("ref %+v", ref)
	}
	if got, _ := os.ReadFile(ref.Path); !bytes.Equal(got, testPDF) {
		t.Fatal("the copy differs")
	}
	if ig, _ := os.ReadFile(filepath.Join(work, "attachments", ".gitignore")); string(ig) != "*\n" {
		t.Fatalf(".gitignore %q", ig)
	}
	// A file gone from the state directory is left out, and holds up
	// neither the instruction nor the rest; a link in the workdir is
	// never written through.
	more, err := storeAttachments(cfg, "widgets", "I2", stage(t, upload{"a.txt", []byte("one\n"), 0}, upload{"b.txt", []byte("two\n"), 0}))
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(attachmentPath(cfg, more[0]))
	outside := filepath.Join(dir, "outside.txt")
	os.WriteFile(outside, []byte("six\n"), 0o600)
	work2 := filepath.Join(dir, "work2")
	os.MkdirAll(work2, 0o755)
	refs, missing := copyAttachments(cfg, work2, more)
	if len(refs) != 1 || refs[0].ID != more[1].ID || len(missing) != 1 || missing[0] != more[0].ID {
		t.Fatalf("refs %+v missing %v", refs, missing)
	}
	linked := filepath.Join(dir, "work3")
	os.MkdirAll(linked, 0o755)
	os.Symlink(dir, filepath.Join(linked, "attachments"))
	if refs, missing := copyAttachments(cfg, linked, more[1:]); len(refs) != 0 || len(missing) != 1 {
		t.Fatalf("written through a link: %+v", refs)
	}
	os.MkdirAll(filepath.Join(dir, "work4", "attachments"), 0o755)
	os.Symlink(outside, filepath.Join(dir, "work4", "attachments", more[1].Hash+".txt"))
	if refs, _ := copyAttachments(cfg, filepath.Join(dir, "work4"), more[1:]); len(refs) != 0 {
		t.Fatalf("a link taken for the copy: %+v", refs)
	}
	if !strings.Contains(string(b), `"attachments":[{"id":"`+atts[0].ID+`","name":"spec.pdf","type":"application/pdf","bytes":`) {
		t.Fatalf("the record's shape:\n%s", b)
	}
}

// stage checks files as a POST's, through a real multipart form.
func stage(t *testing.T, files ...upload) []stagedAttachment {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, f := range files {
		w, _ := mw.CreateFormFile("file", f.name)
		w.Write(f.body)
	}
	mw.Close()
	form, err := multipart.NewReader(&b, mw.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { form.RemoveAll() })
	staged, err := checkAttachments(form.File["file"])
	if err != nil {
		t.Fatal(err)
	}
	return staged
}

// A screenshot pasted into the repository's box reaches the thread agent
// by name and path, never inlined: its input lists it, and it may read
// that one directory beside its clone, which it may not without one.
func TestThreadAgentGetsAttachment(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	h.cfg.EventLog = filepath.Join(t.TempDir(), "events.jsonl")
	h.sup.cfg.EventLog = h.cfg.EventLog
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 1, agentOut("The screenshot shows a red dot on a white field.", ""))
	shot := testPNG(t)
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "", "What does this show?", stage(t, upload{"image.png", shot, 0})); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	x := agentInput(t, dir, 1)
	attDir := attachmentsDir(h.cfg, "widgets")
	if len(x.Message.Attachments) != 1 {
		t.Fatalf("the agent's message = %+v", x.Message)
	}
	ref := x.Message.Attachments[0]
	if ref.Name != "image.png" || ref.Type != "image/png" || filepath.Dir(ref.Path) != attDir || ref.Bytes != int64(len(shot)) {
		t.Fatalf("ref %+v", ref)
	}
	if got, _ := os.ReadFile(ref.Path); !bytes.Equal(got, shot) {
		t.Fatal("the path holds no screenshot")
	}
	in, _ := os.ReadFile(filepath.Join(dir, "in.1"))
	if bytes.Contains(in, []byte("PNG")) || len(in) > 4096 {
		t.Fatalf("the image is inlined:\n%s", in)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.1"))
	read := "Read(//" + strings.TrimPrefix(attDir, "/") + "/**)"
	if !strings.Contains(string(args), "--add-dir\n"+attDir+"\n") || !strings.Contains(string(args), read) {
		t.Fatalf("the agent may not read the attachments:\n%s", args)
	}
	if in := h.instruction(t); in.Reply != "The screenshot shows a red dot on a white field." {
		t.Fatalf("reply %q", in.Reply)
	}
	// A thread without a file is given no such access.
	claudeAnswers(t, dir, 2, agentOut("Fine.", ""))
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "", "And now?", nil); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if args, _ := os.ReadFile(filepath.Join(dir, "args.2")); strings.Contains(string(args), "--add-dir") || strings.Contains(string(args), attDir) {
		t.Fatalf("a thread without files may read them:\n%s", args)
	}
}

// An attachment is a history record on its message's topics (#60), by
// name, type and path, never its bytes.
func TestAttachmentHistory(t *testing.T) {
	cfg := &Config{EventLog: filepath.Join(t.TempDir(), "events.jsonl"), Repositories: map[string]Repository{"widgets": {}}}
	src := newHistSource(cfg)
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	in := Event{V: EventVersion, Time: at, Kind: KindInstruction, Mode: ModeOwner, Instruction: &InstructionPayload{ID: "I1", Repositories: []string{"widgets"}, Text: "This breaks #12", From: "ui", To: "w1"}}
	a := AttachmentPayload{ID: "Yabc", Message: "I1", Repository: "widgets", Name: "shot.png", Type: "image/png", Bytes: 99, Hash: strings.Repeat("ab", 32)}
	if got := src.records(in); len(got) != 1 {
		t.Fatalf("instruction records %+v", got)
	}
	got := src.records(Event{V: EventVersion, Time: at, Kind: KindAttachment, Mode: ModeOwner, Attachment: &a})
	if len(got) != 1 {
		t.Fatalf("attachment records %+v", got)
	}
	r := got[0]
	if r.ID != "Yabc" || r.Kind != HistAttachment || r.Title != "shot.png" || !contains(r.Topics, "#12") || !contains(r.Topics, "session:w1") ||
		!strings.Contains(r.Text, attachmentPath(cfg, a)) || r.repo != "widgets" {
		t.Fatalf("record %+v", r)
	}
	// One of a message never read is on nothing.
	b := a
	b.Message = "I9"
	if got := src.records(Event{V: EventVersion, Time: at, Kind: KindAttachment, Mode: ModeOwner, Attachment: &b}); len(got) != 0 {
		t.Fatalf("records %+v", got)
	}
}

// An attachment event is the owner's and names a stored file; one that
// does not is refused before it is logged.
func TestAttachmentEventChecked(t *testing.T) {
	good := AttachmentPayload{ID: "Y1", Message: "I1", Repository: "widgets", Name: "a.png", Type: "image/png", Bytes: 9, Hash: strings.Repeat("0", 64)}
	ev := func(a AttachmentPayload, mode, session string) Event {
		return Event{V: EventVersion, Time: time.Now(), Kind: KindAttachment, Mode: mode, Session: session, Attachment: &a}
	}
	if err := ev(good, ModeOwner, "").check(); err != nil {
		t.Fatal(err)
	}
	bad := []Event{ev(good, ModeLive, ""), ev(good, ModeOwner, "w1")}
	for _, change := range []func(*AttachmentPayload){
		func(a *AttachmentPayload) { a.Type = "text/html" },
		func(a *AttachmentPayload) { a.Hash = "../x" },
		func(a *AttachmentPayload) { a.Bytes = maxAttachment + 1 },
		func(a *AttachmentPayload) { a.Message = "" },
	} {
		a := good
		change(&a)
		bad = append(bad, ev(a, ModeOwner, ""))
	}
	for i, e := range bad {
		if err := e.check(); err == nil {
			t.Errorf("%d: %+v passed", i, e.Attachment)
		}
	}
	if d := attachmentsDir(&Config{EventLog: "/s/events.jsonl"}, "../etc"); filepath.Dir(d) != "/s/attachments" {
		t.Fatalf("a name reaches outside: %s", d)
	}
}

// A name is kept as its base name, without control characters, and one
// that matches a confidential pattern as its type's generic name.
func TestAttachmentNames(t *testing.T) {
	for in, want := range map[string]string{"C:\\Users\\x\\shot.png": "shot.png", "../../a\x00b.txt": "ab.txt", "": "file", strings.Repeat("é", 200): strings.Repeat("é", attachmentName)} {
		if got := cleanAttachmentName(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	cfg := &Config{EventLog: filepath.Join(t.TempDir(), "events.jsonl"), ConfidentialPatterns: []string{"secret-[a-z]+"}}
	atts, err := storeAttachments(cfg, "widgets", "I1", stage(t, upload{"secret-plan.png", testPNG(t), 0}))
	if err != nil || atts[0].Name != "attachment.png" {
		t.Fatalf("%v %+v", err, atts)
	}
}
