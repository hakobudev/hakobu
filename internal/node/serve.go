package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"sync"
)

// A node on another server serves the panel its Node over the link
// (internal/link): every method is POST /call/<Method> with its arguments
// as JSON, answered by a stream of frames, one JSON line each: output (a
// deploy's log, say), events (containers dying), then the results or the
// error. Remote is the panel's side. The node asks the panel back for a
// backup's presigned URLs over the same link.

// frame is one line of a call's answer.
type frame struct {
	T string            `json:"t"`           // "out", "event", "result" or "error"
	D string            `json:"d,omitempty"` // output
	E *deathEvent       `json:"e,omitempty"` // event
	R []json.RawMessage `json:"r,omitempty"` // results, error left out
	M string            `json:"m,omitempty"` // error message
	C string            `json:"c,omitempty"` // error code
}

type deathEvent struct {
	Container, App string
	Death          Death
}

// The errors the panel tells apart, by code.
var errorCodes = map[string]error{
	"previous_not_kept": ErrPreviousNotKept,
}

// wireUpload and wireDownload carry an Upload or Download with the ID of
// the panel's callback that makes its URLs.
type wireUpload struct {
	Upload
	Callback string
}

type wireDownload struct {
	Download
	Callback string
}

var (
	contextType  = reflect.TypeFor[context.Context]()
	writerType   = reflect.TypeFor[io.Writer]()
	errorType    = reflect.TypeFor[error]()
	uploadType   = reflect.TypeFor[Upload]()
	downloadType = reflect.TypeFor[Download]()
	deathFnType  = reflect.TypeFor[func(container, app string, d Death)]()
)

// Serve answers the panel's calls on the link's streams with n until the
// link ends; dial opens a stream to the panel, for its callbacks.
func Serve(l net.Listener, n Node, dial func() (net.Conn, error)) error {
	return (&http.Server{Handler: Handler(n, dial)}).Serve(l)
}

// PanelMoved, on a node, keeps the panel's new address ("https://host")
// the panel sends when it moves, for connecting next time.
var PanelMoved func(url string) error

// Handler is the HTTP side of Serve.
func Handler(n Node, dial func() (net.Conn, error)) http.Handler {
	panel := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return dial() },
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /panel", func(w http.ResponseWriter, r *http.Request) {
		var a struct{ URL string }
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if PanelMoved == nil {
			http.Error(w, "this server keeps no panel address", http.StatusNotFound)
			return
		}
		if err := PanelMoved(a.URL); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /call/{method}", func(w http.ResponseWriter, r *http.Request) {
		m := reflect.ValueOf(n).MethodByName(r.PathValue("method"))
		if !m.IsValid() {
			http.Error(w, "no such method", http.StatusNotFound)
			return
		}
		var args []json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f := &frames{w: w}
		in, err := callArgs(r.Context(), m.Type(), args, f, panel)
		if err != nil {
			f.send(frame{T: "error", M: err.Error()})
			return
		}
		var results []json.RawMessage
		var callErr error
		for _, v := range m.Call(in) {
			if v.Type() == errorType {
				if !v.IsNil() {
					callErr = v.Interface().(error)
				}
				continue
			}
			b, err := json.Marshal(v.Interface())
			if err != nil {
				callErr = err
				break
			}
			results = append(results, b)
		}
		if callErr != nil {
			fr := frame{T: "error", M: callErr.Error()}
			for code, sentinel := range errorCodes {
				if errors.Is(callErr, sentinel) {
					fr.C = code
				}
			}
			f.send(fr)
			return
		}
		f.send(frame{T: "result", R: results})
	})
	return mux
}

// callArgs builds the method's arguments: its context is the request's,
// its output and events become frames, an Upload's or Download's URLs
// are asked of the panel, the rest are decoded in order.
func callArgs(ctx context.Context, t reflect.Type, args []json.RawMessage, f *frames, panel *http.Client) ([]reflect.Value, error) {
	var in []reflect.Value
	next := func() (json.RawMessage, error) {
		if len(args) == 0 {
			return nil, errors.New("too few arguments")
		}
		a := args[0]
		args = args[1:]
		return a, nil
	}
	for i := range t.NumIn() {
		p := t.In(i)
		switch p {
		case contextType:
			in = append(in, reflect.ValueOf(ctx))
		case writerType:
			in = append(in, reflect.ValueOf(io.Writer(outWriter{f})))
		case deathFnType:
			in = append(in, reflect.ValueOf(func(container, app string, d Death) {
				f.send(frame{T: "event", E: &deathEvent{container, app, d}})
			}))
		case uploadType:
			a, err := next()
			if err != nil {
				return nil, err
			}
			var u wireUpload
			if err := json.Unmarshal(a, &u); err != nil {
				return nil, err
			}
			u.URL = callback(ctx, panel, u.Callback)
			in = append(in, reflect.ValueOf(u.Upload))
		case downloadType:
			a, err := next()
			if err != nil {
				return nil, err
			}
			var d wireDownload
			if err := json.Unmarshal(a, &d); err != nil {
				return nil, err
			}
			d.URL = callback(ctx, panel, d.Callback)
			in = append(in, reflect.ValueOf(d.Download))
		default:
			a, err := next()
			if err != nil {
				return nil, err
			}
			v := reflect.New(p)
			if err := json.Unmarshal(a, v.Interface()); err != nil {
				return nil, fmt.Errorf("argument %d: %w", i, err)
			}
			in = append(in, v.Elem())
		}
	}
	return in, nil
}

// callback asks the panel for the URL of a backup's part.
func callback(ctx context.Context, panel *http.Client, id string) func(int) (string, error) {
	return func(part int) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://panel/callback/"+id+"?part="+strconv.Itoa(part), nil)
		if err != nil {
			return "", err
		}
		resp, err := panel.Do(req)
		if err != nil {
			return "", fmt.Errorf("asking the panel for a backup's URL: %w", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("the panel gave no URL for part %d: %s", part, b)
		}
		return string(b), nil
	}
}

// frames writes a call's answer, flushed frame by frame so the panel sees
// output as it comes.
type frames struct {
	mu sync.Mutex
	w  http.ResponseWriter
}

func (f *frames) send(fr frame) {
	b, _ := json.Marshal(fr)
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.w.Write(append(b, '\n'))
	_ = http.NewResponseController(f.w).Flush()
}

type outWriter struct{ f *frames }

func (o outWriter) Write(p []byte) (int, error) {
	o.f.send(frame{T: "out", D: string(p)})
	return len(p), nil
}

// readFrames reads a call's answer, passing output to out and events to
// onEvent, and returns the final frame.
func readFrames(r io.Reader, out io.Writer, onEvent func(deathEvent)) (frame, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var fr frame
		if err := json.Unmarshal(sc.Bytes(), &fr); err != nil {
			return frame{}, err
		}
		switch fr.T {
		case "out":
			if out != nil {
				_, _ = io.WriteString(out, fr.D)
			}
		case "event":
			if onEvent != nil && fr.E != nil {
				onEvent(*fr.E)
			}
		default:
			return fr, nil
		}
	}
	if err := sc.Err(); err != nil {
		return frame{}, err
	}
	return frame{}, io.ErrUnexpectedEOF
}
