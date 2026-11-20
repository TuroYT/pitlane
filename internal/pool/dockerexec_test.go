package pool

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

// fakeExecs answers the exec calls of the containers it serves: run is
// called with the container and script of each exec and returns its output
// and exit code. A negative code makes the exec creation fail with a 500.
type fakeExecs struct {
	mu      sync.Mutex
	run     func(container, script string) (out string, code int)
	scripts []string // "container: script", in order
	results map[string]int
	outputs map[string]string
}

// execRoutes returns the routes serving exec for the named containers.
func execRoutes(t *testing.T, run func(container, script string) (string, int), names ...string) (map[string]http.HandlerFunc, *fakeExecs) {
	t.Helper()
	f := &fakeExecs{run: run, results: map[string]int{}, outputs: map[string]string{}}
	routes := map[string]http.HandlerFunc{}
	for _, name := range names {
		routes["POST /containers/"+name+"/exec"] = f.create(name)
	}
	// Exec ids are handed out in order: e0, e1…
	for i := range 64 {
		id := "e" + strconv.Itoa(i)
		routes["POST /exec/"+id+"/start"] = f.start(id)
		routes["GET /exec/"+id+"/json"] = f.inspect(id)
	}
	return routes, f
}

func (f *fakeExecs) create(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Cmd []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		script := ""
		if len(body.Cmd) == 3 {
			script = body.Cmd[2]
		}
		out, code := f.run(name, script)
		f.mu.Lock()
		id := "e" + strconv.Itoa(len(f.scripts))
		f.scripts = append(f.scripts, name+": "+script)
		f.results[id], f.outputs[id] = code, out
		f.mu.Unlock()
		if code < 0 {
			replyStatus(http.StatusInternalServerError)(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	}
}

func (f *fakeExecs) start(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // closing with unread input would reset the connection
		f.mu.Lock()
		out := f.outputs[id]
		f.mu.Unlock()
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		writeFrame(buf, out)
		_ = buf.Flush()
	}
}

func (f *fakeExecs) inspect(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		code := f.results[id]
		f.mu.Unlock()
		reply(map[string]any{"ID": id, "Running": false, "ExitCode": code})(w, r)
	}
}

// writeFrame writes s as one stdout frame of Docker's multiplexed stream.
func writeFrame(w *bufio.ReadWriter, s string) {
	if s == "" {
		return
	}
	hdr := make([]byte, 8)
	hdr[0] = 1
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(s)))
	_, _ = w.Write(hdr)
	_, _ = io.WriteString(w, s)
}

// ran returns the "container: script" of every exec done so far.
func (f *fakeExecs) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.scripts...)
}
