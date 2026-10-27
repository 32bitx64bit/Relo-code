package art

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testKey = "sk-test-SECRET0123456789"

// call is one request the fake API received.
type call struct {
	Path        string
	ContentType string
	Auth        string
	Body        []byte
	N           int
	Fields      map[string]string
	Images      int // image[] parts of an edit
	ImageTypes  []string
}

// fakeAPI stands in for an OpenAI-compatible image endpoint.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	calls []call
	// handler answers a request; nil gives pictures and usage.
	handler func(w http.ResponseWriter, r *http.Request, c call)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) endpoint() Endpoint { return Endpoint{BaseURL: f.srv.URL + "/v1/", Key: testKey} }

func (f *fakeAPI) setHandler(h func(w http.ResponseWriter, r *http.Request, c call)) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAPI) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		f.t.Fatal("the API received no request")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	c := call{Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), Auth: r.Header.Get("Authorization"), Fields: map[string]string{}}
	body, _ := io.ReadAll(r.Body)
	c.Body = body
	if strings.HasPrefix(c.ContentType, "application/json") {
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		for k, v := range m {
			c.Fields[k] = fmt.Sprint(v)
		}
	} else if strings.HasPrefix(c.ContentType, "multipart/form-data") {
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if err := r.ParseMultipartForm(64 << 20); err == nil {
			for k, v := range r.MultipartForm.Value {
				c.Fields[k] = v[0]
			}
			for _, fh := range r.MultipartForm.File["image[]"] {
				c.Images++
				c.ImageTypes = append(c.ImageTypes, fh.Header.Get("Content-Type"))
			}
		}
	}
	c.N, _ = strconv.Atoi(c.Fields["n"])
	f.mu.Lock()
	f.calls = append(f.calls, c)
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h(w, r, c)
		return
	}
	f.respond(w, c.N, true)
}

// respond writes n distinct pictures, with usage when asked: 100 text and 200 image tokens in, 1056 out, which
// costs $0.03378 at the default model's rates.
func (f *fakeAPI) respond(w http.ResponseWriter, n int, usage bool) {
	if n < 1 {
		n = 1
	}
	var data []map[string]string
	for i := 0; i < n; i++ {
		png := discPNG(f.t, 64+8*i, 64+8*i, color.NRGBA{0, 0, 0, 0})
		data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(png)})
	}
	out := map[string]any{"data": data}
	if usage {
		out["usage"] = map[string]any{"input_tokens": 300, "output_tokens": 1056, "input_tokens_details": map[string]int{"text_tokens": 100, "image_tokens": 200}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func apiError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error"}})
}
