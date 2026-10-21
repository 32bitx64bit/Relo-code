package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverOpenAI(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"data":[{"id":"zeta"},{"id":"alpha","context_length":32000},{"id":"mid","max_model_len":8192},{"id":"alpha"},{"id":"bad id"}]}`)
	}))
	defer srv.Close()
	models, err := Discover(context.Background(), KindOpenAI, srv.URL+"/v1/models/", "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v, want alpha, mid, zeta (dup and invalid id dropped)", models)
	}
	if models[0].ID != "alpha" || models[0].Context != 32000 || models[1].Context != 8192 || models[2].ID != "zeta" {
		t.Fatalf("models = %+v, want sorted with context fallbacks", models)
	}
}

func TestDiscoverOpenAINoKeyOmitsHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Errorf("Authorization sent without key")
		}
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), KindOpenAI, srv.URL, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverAnthropicPaginationAndEffort(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k-1" || r.Header.Get("anthropic-version") != "2023-06-01" {
			http.Error(w, "missing headers", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("limit") != "1000" {
			t.Errorf("limit = %q", r.URL.Query().Get("limit"))
		}
		after := r.URL.Query().Get("after_id")
		pages = append(pages, after)
		switch after {
		case "":
			fmt.Fprint(w, `{"data":[{"id":"claude-b","display_name":"Claude B","max_input_tokens":200000,"max_tokens":64000,
				"capabilities":{"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":false},"max":{"supported":true}},"thinking":{"supported":true}}}],
				"has_more":true,"last_id":"claude-b"}`)
		case "claude-b":
			fmt.Fprint(w, `{"data":[{"id":"claude-a","display_name":"Claude A","max_input_tokens":100000,"max_tokens":8000,
				"capabilities":{"effort":{"supported":false,"low":{"supported":true}},"thinking":{"supported":false}}}],"has_more":false,"last_id":"claude-a"}`)
		default:
			t.Errorf("unexpected page %q", after)
		}
	}))
	defer srv.Close()
	models, err := Discover(context.Background(), KindAnthropic, srv.URL+"/v1/models", "k-1")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pages) != "[ claude-b]" {
		t.Fatalf("pages = %v", pages)
	}
	if len(models) != 2 || models[0].ID != "claude-a" || models[1].ID != "claude-b" {
		t.Fatalf("models = %+v", models)
	}
	a, b := models[0], models[1]
	if a.Context != 100000 || a.Output != 8000 || a.Efforts != nil || a.Reasoning {
		t.Fatalf("claude-a = %+v, want no efforts when effort.supported is false", a)
	}
	if b.Name != "Claude B" || b.Context != 200000 || b.Output != 64000 || !b.Reasoning {
		t.Fatalf("claude-b = %+v", b)
	}
	if fmt.Sprint(b.Efforts) != "[low medium high max]" {
		t.Fatalf("claude-b efforts = %v", b.Efforts)
	}
}

func TestDiscoverAnthropicPaginationBounded(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"data":[{"id":"m%d"}],"has_more":true,"last_id":"m%d"}`, calls, calls)
	}))
	defer srv.Close()
	models, err := Discover(context.Background(), KindAnthropic, srv.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if calls != discoverMaxPages || len(models) != discoverMaxPages {
		t.Fatalf("calls = %d models = %d, want %d", calls, len(models), discoverMaxPages)
	}
}

func TestDiscoverErrorsAreScrubbed(t *testing.T) {
	const key = "sk-very-secret-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid key `+key+`"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := Discover(context.Background(), KindOpenAI, srv.URL, key)
	if err == nil {
		t.Fatal("want error on 401")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error = %q, want status", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("key leaked in error: %q", err)
	}
}

func TestDiscoverBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":`)
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), KindOpenAI, srv.URL, ""); err == nil || !strings.Contains(err.Error(), "bad JSON") {
		t.Fatalf("err = %v, want bad JSON", err)
	}
}

func TestDiscoverOversizeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[],"pad":"`))
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < 5; i++ {
			w.Write([]byte(chunk))
		}
		w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), KindOpenAI, srv.URL, ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want size error", err)
	}
}

func TestDiscoverRejectsRedirectToOtherHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"x"}]}`)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/models", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), KindOpenAI, srv.URL, "sk-x"); err == nil {
		t.Fatal("redirect to another host must be refused")
	}
}

func TestDiscoverValidatesInput(t *testing.T) {
	ctx := context.Background()
	if _, err := Discover(ctx, "gemini", "https://x.test", ""); err == nil {
		t.Fatal("bad kind accepted")
	}
	if _, err := Discover(ctx, KindOpenAI, "ftp://x.test", ""); err == nil {
		t.Fatal("bad scheme accepted")
	}
}

func TestDiscoverReadsImageSupport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"or-sees","architecture":{"input_modalities":["text","image"]}},
			{"id":"or-blind","architecture":{"modality":"text->text"}},
			{"id":"or-old","architecture":{"modality":"text+image->text"}},
			{"id":"flat","input_modalities":["text"]},
			{"id":"nested","modalities":{"input":["image","text"]}},
			{"id":"ollama","capabilities":["completion","vision"]},
			{"id":"ollama-text","capabilities":["completion","tools"]},
			{"id":"obj","capabilities":{"vision":false}},
			{"id":"litellm","supports_vision":true},
			{"id":"odd","architecture":"x","capabilities":7,"supports_vision":"yes"},
			{"id":"plain"}]}`)
	}))
	defer srv.Close()
	models, err := Discover(context.Background(), KindOpenAI, srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"or-sees": "yes", "or-blind": "no", "or-old": "yes", "flat": "no", "nested": "yes", "ollama": "yes", "ollama-text": "", "obj": "no", "litellm": "yes", "odd": "", "plain": ""}
	if len(models) != len(want) {
		t.Fatalf("a model with odd fields was dropped: %d models", len(models))
	}
	for _, m := range models {
		got := ""
		if m.Vision != nil {
			got = map[bool]string{true: "yes", false: "no"}[*m.Vision]
		}
		if got != want[m.ID] {
			t.Errorf("%s: vision %q, want %q", m.ID, got, want[m.ID])
		}
	}
}

func TestDiscoverAnthropicImageInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"claude-sees","capabilities":{"image_input":{"supported":true}}},
			{"id":"claude-blind","capabilities":{"image_input":{"supported":false}}},
			{"id":"claude-loose","capabilities":{"image_input":true,"thinking":true}},
			{"id":"claude-silent","capabilities":{}}],"has_more":false}`)
	}))
	defer srv.Close()
	models, err := Discover(context.Background(), KindAnthropic, srv.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range models {
		got[m.ID] = "unknown"
		if m.Vision != nil {
			got[m.ID] = fmt.Sprint(*m.Vision)
		}
		if m.ID == "claude-loose" && !m.Reasoning {
			t.Error("a bare-bool thinking flag was not read")
		}
	}
	if fmt.Sprint(got) != "map[claude-blind:false claude-loose:true claude-sees:true claude-silent:unknown]" {
		t.Fatalf("image_input read as %v", got)
	}
}
