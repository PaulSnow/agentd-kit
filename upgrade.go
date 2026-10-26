package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// A session's tool server outlives an upgrade of agentd (#45): the
// session started agentd mcp on the old binary and keeps it, and once the
// log holds an event the old reader does not know, every read fails. The
// server notes its binary when it starts; a read that fails after the
// binary on disk was replaced is an older build reading a newer log, so
// the server re-executes the binary on disk, in place, keeping its
// standard input and output. The call it has not answered, and the input
// it has read past, go to the new server in a handover file, and the new
// server's first tool reply says it restarted. A read that fails with the
// binary unchanged is refused as before, saying to restart the session.
// So is one after an install that switches a symbolic link: the session
// runs the path the link resolved to, which still holds the old build.
// A downgrade to a build older than this hand-over drops the call it was
// handed, and its file stays in the temporary directory.

// mcpHandoverEnv names the handover file of a re-executed server.
const mcpHandoverEnv = "AGENTD_MCP_HANDOVER"

// mcpHandover is what an old server hands the new one: the input it read
// and did not answer, and why it restarted.
type mcpHandover struct {
	Input []byte `json:"input"`
	Why   string `json:"why"`
}

// load reads the log for a call. A read that fails once the binary was
// replaced leaves the call to the binary on disk (Serve), unless
// re-executing it already failed.
func (srv *MCPServer) load() ([]Event, error) {
	events, err := srv.read()
	if err == nil {
		return events, nil
	}
	if srv.replaced != nil && srv.replaced() {
		if srv.restartErr != nil {
			return nil, fmt.Errorf("%v; agentd was upgraded, and re-executing its tool server failed (%v): restart this session", err, srv.restartErr)
		}
		if srv.restart != nil {
			srv.restarting = err.Error()
		}
	}
	return nil, fmt.Errorf("%v; if agentd was upgraded, restart this session", err)
}

// restartNote is what the new server says in its first tool reply.
func restartNote(why string) string {
	return "agentd's tool server was on an older build than its log (" + cutUTF8(why, 300) + ") and restarted on the current one"
}

// withField is the compact JSON object reply with key set to value.
func withField(reply, key, value string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(reply), &m) != nil {
		return reply
	}
	v, _ := json.Marshal(value)
	m[key] = v
	return compact(m)
}

// buffered is the input br has read past the line it returned.
func buffered(br *bufio.Reader) []byte {
	b, _ := br.Peek(br.Buffered())
	return b
}

// replacedSince notes the binary running, image, or, when that cannot be
// read, the one at exe, and says, later, whether another file is at exe
// now: an install moves a new build over the old. Nil when neither can be
// read.
func replacedSince(exe, image string) func() bool {
	was, err := os.Stat(image)
	if err != nil {
		if was, err = os.Stat(exe); err != nil {
			return nil
		}
	}
	return func() bool {
		now, err := os.Stat(exe)
		return err == nil && (!os.SameFile(was, now) || now.Size() != was.Size() || !now.ModTime().Equal(was.ModTime()))
	}
}

// reexecMCP replaces the process with exe run with args, handing it the
// pending input. It returns only when it cannot.
func reexecMCP(exe string, args []string, pending []byte, why string) error {
	f, err := os.CreateTemp("", "agentd-mcp-*.json")
	if err != nil {
		return err
	}
	b, _ := json.Marshal(mcpHandover{Input: pending, Why: why})
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, mcpHandoverEnv+"=") {
				env = append(env, kv)
			}
		}
		err = syscall.Exec(exe, append([]string{exe}, args...), append(env, mcpHandoverEnv+"="+f.Name()))
	}
	os.Remove(f.Name())
	return err
}

// takeHandover reads and removes the handover a re-executed server was
// started with, if any.
func takeHandover() ([]byte, string, error) {
	path := os.Getenv(mcpHandoverEnv)
	if path == "" {
		return nil, "", nil
	}
	os.Unsetenv(mcpHandoverEnv)
	b, err := os.ReadFile(path)
	os.Remove(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading the handover of the tool server's restart: %w", err)
	}
	var h mcpHandover
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, "", fmt.Errorf("the handover of the tool server's restart is malformed: %w", err)
	}
	return h.Input, h.Why, nil
}
