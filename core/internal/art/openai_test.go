package art

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveDescriptors are supported_parameters descriptors in the shape GET /images/models returns (type enum with values,
// range with min and max, boolean). Enum value lists are trimmed to the values the tests send.
var liveDescriptors = map[string]string{
	"aspect_ratio":     `{"type":"enum","values":["1:1","3:2","16:9"]}`,
	"resolution":       `{"type":"enum","values":["1K","2K"]}`,
	"quality":          `{"type":"enum","values":["auto","low","medium","high"]}`,
	"background":       `{"type":"enum","values":["auto","transparent","opaque"]}`,
	"output_format":    `{"type":"enum","values":["png","svg"]}`,
	"input_references": `{"type":"range","min":0,"max":4}`,
	"n":                `{"type":"range","min":1,"max":6}`,
	"size":             `{"type":"boolean"}`,
	"seed":             `{"type":"boolean"}`,
}

// descriptorsFor builds a supported_parameters object from liveDescriptors for the named parameters.
func descriptorsFor(names ...string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%q:%s", n, liveDescriptors[n]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// liveListing is a GET /images/models body with one model whose supported_parameters is params (a JSON object).
func liveListing(model, params string) string {
	return fmt.Sprintf(`{"data":[{"id":%q,"supported_parameters":%s}]}`, model, params)
}

func TestOpenRouterOptionalParametersAndPreflight(t *testing.T) {
	seed := int64(0)
	for _, missing := range []string{"", "aspect_ratio", "resolution", "seed"} {
		t.Run("missing_"+missing, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					params := []string{}
					for _, p := range []string{"aspect_ratio", "resolution", "seed"} {
						if p != missing {
							params = append(params, p)
						}
					}
					fmt.Fprint(w, liveListing("m", descriptorsFor(params...)))
					return
				}
				posts++
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["aspect_ratio"] != "16:9" || body["resolution"] != "2K" || body["seed"] != float64(0) {
					t.Error("incorrect optional parameters")
				}
				fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
			}))
			defer server.Close()
			_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "m", AspectRatio: "16:9", Resolution: "2K", Seed: &seed, recovered: true})
			if missing == "" && (err != nil || posts != 1) {
				t.Fatalf("err=%v posts=%d", err, posts)
			}
			if missing != "" && (err == nil || !strings.Contains(err.Error(), missing) || posts != 0) {
				t.Fatalf("err=%v posts=%d", err, posts)
			}
		})
	}
	body, _, _ := referenceBody(imageCall{Model: "m"}, nil, 0)
	for _, p := range []string{"aspect_ratio", "resolution", "seed"} {
		if bytes.Contains(body, []byte(p)) {
			t.Errorf("unset parameter %s sent", p)
		}
	}
	for _, c := range []imageCall{{AspectRatio: "1:1"}, {Resolution: "2K"}, {Seed: &seed}} {
		_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: "http://127.0.0.1"}, c)
		if err == nil || !strings.Contains(err.Error(), "only supported for OpenRouter") {
			t.Fatalf("non-OpenRouter error=%v", err)
		}
	}
}

func TestOpenRouterMediaTypesAnd400Diagnostic(t *testing.T) {
	_, err := decodeRefReply([]byte(`{"data":[{"b64_json":"YQ==","media_type":"image/svg+xml"}]}`), 200)
	if err == nil || !strings.Contains(err.Error(), "image/svg+xml") {
		t.Fatalf("media error=%v", err)
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 3, 3)), nil); err != nil {
		t.Fatal(err)
	}
	webp, err := os.ReadFile("../vision/testdata/gopher.webp")
	if err != nil {
		t.Fatal(err)
	}
	for typ, data := range map[string][]byte{"image/jpeg": jpg.Bytes(), "image/webp": webp} {
		t.Run(typ, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(data), "media_type": typ}}})
			reply, err := decodeRefReply(body, 200)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := prepare(reply.Images[0], maxCandidateEdge)
			if err != nil || !bytes.HasPrefix(stored.PNG, []byte("\x89PNG\r\n\x1a\n")) {
				t.Fatalf("conversion failed: %v", err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, liveListing("m", descriptorsFor("seed")))
			return
		}
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"seed rejected; resolution ignored"}}`)
	}))
	defer server.Close()
	seed := int64(1)
	_, err = callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "m", Seed: &seed})
	if err == nil || !strings.Contains(err.Error(), "seed") || strings.Contains(err.Error(), "resolution") {
		t.Fatalf("diagnostic=%v", err)
	}
}

func TestOpenRouterReferenceJobUsesImagesEndpoint(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.Provider = "openrouter" })
	first, err := r.m.Store().AddRef(discPNG(t, 50, 50, color.NRGBA{255, 0, 0, 255}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.m.Store().AddRef(discPNG(t, 40, 40, color.NRGBA{0, 0, 255, 255}))
	if err != nil {
		t.Fatal(err)
	}
	firstData, err := readPicture(filepath.Join(r.m.Store().RefsDir(), first.ID+".png"))
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := readPicture(filepath.Join(r.m.Store().RefsDir(), second.ID+".png"))
	if err != nil {
		t.Fatal(err)
	}
	outputs := [][]byte{discPNG(t, 32, 32, color.NRGBA{0, 255, 0, 255}), discPNG(t, 36, 36, color.NRGBA{255, 255, 0, 255})}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/images/models" {
			fmt.Fprint(w, liveListing(DefaultModel, descriptorsFor("input_references")))
			return
		}
		calls++
		if req.Method != http.MethodPost || req.URL.Path != "/v1/images" || req.Header.Get("Authorization") != "Bearer "+testKey {
			t.Errorf("unexpected request: %s %s or authorization", req.Method, req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["model"] != DefaultModel || body["prompt"] != "match refs" || body["n"] != nil || body["messages"] != nil || body["modalities"] != nil {
			t.Errorf("unexpected body: %+v", body)
			return
		}
		refs, _ := body["input_references"].([]any)
		if len(refs) != 2 {
			t.Errorf("input_references = %+v", body["input_references"])
			return
		}
		for i, want := range [][]byte{firstData, secondData} {
			obj, _ := refs[i].(map[string]any)
			img, _ := obj["image_url"].(map[string]any)
			if len(obj) != 2 || obj["type"] != "image_url" || len(img) != 1 || img["url"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(want) {
				t.Errorf("reference %d out of order or incorrect", i)
			}
		}
		fmt.Fprintf(w, `{"data":[{"b64_json":"%s"}]}`, base64.StdEncoding.EncodeToString(outputs[calls-1]))
	}))
	t.Cleanup(server.Close)
	r.endpoint = func(string) (Endpoint, error) { return Endpoint{BaseURL: server.URL + "/v1", Key: testKey}, nil }
	j := r.finish(r.generate(GenerateInput{Prompt: "match refs", Refs: []string{first.ID, second.ID}, Count: 2}).ID)
	if j.State != StateReady || len(j.Candidates) != 2 || calls != 2 || !j.CostFromEstimate {
		t.Fatalf("job=%+v calls=%d", j, calls)
	}
	for i := 1; i <= 2; i++ {
		data, err := r.m.CandidateData(j.ID, i)
		expected, prepErr := prepare(outputs[i-1], maxCandidateEdge)
		if err != nil || prepErr != nil || !bytes.Equal(data, expected.PNG) {
			t.Fatalf("picture %d: data match=%v err=%v prepErr=%v", i, bytes.Equal(data, expected.PNG), err, prepErr)
		}
	}
}

func TestOpenRouterReferenceReplyAcceptsDataURL(t *testing.T) {
	out := discPNG(t, 24, 24, color.NRGBA{0, 255, 255, 255})
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) {
		fmt.Fprintf(w, `{"data":[{"url":"data:image/png;base64,%s"}]}`, base64.StdEncoding.EncodeToString(out))
	})
	ep := api.endpoint()
	ep.Provider = "openrouter"
	reply, err := callImages(context.Background(), newClient(), ep, imageCall{Model: "m", Prompt: "p", N: 1, Refs: [][]byte{[]byte("ref")}})
	if err != nil || len(reply.Images) != 1 || !bytes.Equal(reply.Images[0], out) || api.last().Path != "/v1/images" {
		t.Fatalf("reply=%+v err=%v call=%+v", reply, err, api.last())
	}
}

func TestOpenRouterReferenceErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"missing route", 404, `{"error":{"message":"missing route"}}`, "OpenRouter has no image route"},
		{"unsupported", 400, `{"error":{"message":"images unsupported"}}`, "OpenRouter rejected the request as invalid"},
		{"unsupported media", 415, `{"error":{"message":"bad type"}}`, "OpenRouter refused the request"},
		{"not implemented", 501, `{"error":{"message":"no refs"}}`, "OpenRouter refused the request"},
		{"no images", 200, `{"data":[]}`, "no picture was returned (HTTP 200)"},
		{"chat answer", 200, `{"choices":[{"message":{"content":"cannot do that"}}]}`, "no picture was returned"},
		{"invalid url", 200, `{"data":[{"url":"https://example.com/a.png"}]}`, "is a link, which Relo does not fetch"},
		{"data url not image", 200, `{"data":[{"url":"data:text/plain;base64,YQ=="}]}`, "not valid picture data"},
		{"bad base64", 200, `{"data":[{"b64_json":"!!"}]}`, "not valid picture data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Refs: [][]byte{[]byte("ref")}})
			ce, ok := err.(*CallError)
			if !ok || ce.Status != tc.status || !strings.Contains(ce.Message, tc.want) || ce.Billed != (tc.status == 200) {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestEdit404ReportsReferencesUnsupported(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) {
		apiError(w, http.StatusNotFound, "edits unavailable")
	})
	_, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Refs: [][]byte{[]byte("ref")}})
	if err == nil || !strings.Contains(err.Error(), "reference images are not supported by the provider") || api.last().Path != "/v1/images/edits" {
		t.Fatalf("error = %v, request = %+v", err, api.last())
	}
}

func TestOpenRouterFailureAfterFirstPictureFailsWholeJob(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r) // the model list is only a preflight; the job goes ahead without it
			return
		}
		calls++
		if calls == 2 {
			http.Error(w, "unsupported references", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
	}))
	defer server.Close()
	reply, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{N: 2, Refs: [][]byte{[]byte("ref")}})
	ce, ok := err.(*CallError)
	if !ok || !ce.Billed || ce.Status != 400 || !strings.Contains(ce.Message, "invalid") || len(reply.Images) != 0 || calls != 2 {
		t.Fatalf("reply=%+v error=%#v calls=%d", reply, err, calls)
	}
}

func TestOpenRouterHostDetection(t *testing.T) {
	for _, tc := range []struct {
		base string
		want bool
	}{
		{"https://openrouter.ai/api/v1", true}, {"https://API.OpenRouter.AI:443/api/v1", true}, {"https://openrouter.ai.example.com/v1", false}, {"https://example.com/openrouter.ai", false},
	} {
		if got := isOpenRouter(Endpoint{BaseURL: tc.base}); got != tc.want {
			t.Errorf("%s = %v", tc.base, got)
		}
	}
}

func TestGenerationIsAJSONCallWithTheKeyInTheHeader(t *testing.T) {
	api := newFakeAPI(t)
	reply, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "an iron ingot", Size: "1024x1024", Quality: "low", Background: "transparent", N: 2})
	if err != nil {
		t.Fatal(err)
	}
	c := api.last()
	if c.Path != "/v1/images/generations" || !strings.HasPrefix(c.ContentType, "application/json") || c.Auth != "Bearer "+testKey {
		t.Fatalf("call = %+v", c)
	}
	for k, want := range map[string]string{"model": "m", "prompt": "an iron ingot", "n": "2", "size": "1024x1024", "quality": "low", "background": "transparent", "output_format": "png"} {
		if c.Fields[k] != want {
			t.Errorf("field %s = %q, want %q", k, c.Fields[k], want)
		}
	}
	if len(reply.Images) != 2 || !reply.HasUsage || reply.Usage != (Usage{TextIn: 100, ImageIn: 200, ImageOut: 1056}) {
		t.Fatalf("reply = %d pictures, usage %+v (%v)", len(reply.Images), reply.Usage, reply.HasUsage)
	}
}

func TestAutoValuesAreLeftOutSoTheEndpointChoosesItself(t *testing.T) {
	api := newFakeAPI(t)
	if _, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "p", Size: "auto", Quality: "auto", Background: "auto", N: 1}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"size", "quality", "background"} {
		if v, ok := api.last().Fields[k]; ok {
			t.Errorf("%s=%q was sent for auto", k, v)
		}
	}
}

func TestReferencesMakeAMultipartEditWithImageTypedParts(t *testing.T) {
	api := newFakeAPI(t)
	ref := discPNG(t, 40, 40, color.NRGBA{255, 255, 255, 255})
	if _, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "match image 1", N: 1, Refs: [][]byte{ref, ref}}); err != nil {
		t.Fatal(err)
	}
	c := api.last()
	if c.Path != "/v1/images/edits" || !strings.HasPrefix(c.ContentType, "multipart/form-data") {
		t.Fatalf("call = %+v", c)
	}
	if c.Images != 2 || c.ImageTypes[0] != "image/png" || c.ImageTypes[1] != "image/png" {
		t.Fatalf("image parts = %d %v", c.Images, c.ImageTypes)
	}
	if c.Fields["prompt"] != "match image 1" || c.Fields["model"] != "m" || c.Fields["n"] != "1" {
		t.Fatalf("fields = %v", c.Fields)
	}
}

func TestAnErrorIsExplainedAndNeverEchoesTheKey(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
		billed bool
	}{
		{401, "Incorrect API key provided: " + testKey + ". You can find your key at https://example.test", "rejected the API key", false},
		{403, "nope", "rejected the API key", false},
		{404, "no route", "no image API", false},
		{429, "You exceeded your current quota", "rate limiting or out of credit", false},
		{400, "Invalid value: 'transparent'. Supported values are: 'opaque'.", "transparent", false},
		{500, "boom", "HTTP 500", false},
	} {
		api := newFakeAPI(t)
		api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) { apiError(w, tc.status, tc.body) })
		_, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "p", N: 1})
		ce, ok := err.(*CallError)
		if !ok {
			t.Fatalf("status %d: error %T %v", tc.status, err, err)
		}
		if !strings.Contains(ce.Message, tc.want) || ce.Billed != tc.billed || ce.Status != tc.status {
			t.Errorf("status %d: %+v, want text %q", tc.status, ce, tc.want)
		}
		if strings.Contains(ce.Message, testKey) || strings.Contains(ce.Message, "SECRET") {
			t.Errorf("status %d: the key came back in %q", tc.status, ce.Message)
		}
	}
}

func TestKeyShapedTextIsScrubbedEvenWhenItIsNotOurKey(t *testing.T) {
	got := scrub("bad key sk-proj-abcdef123456 and key-9876543210zz, but not sky-high", "")
	if strings.Contains(got, "abcdef") || strings.Contains(got, "9876543210") || !strings.Contains(got, "sky-high") {
		t.Fatalf("scrubbed = %q", got)
	}
}

func TestLinksInsteadOfPictureDataAreRefused(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) {
		fmt.Fprint(w, `{"data":[{"url":"https://elsewhere.test/a.png"}]}`)
	})
	_, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "p", N: 1})
	if err == nil || !strings.Contains(err.Error(), "links") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnAnswerThatNeverArrivedMayStillBeBilled(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	_, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "p", N: 1})
	ce, ok := err.(*CallError)
	if !ok || !ce.Billed {
		t.Fatalf("err = %#v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) { <-r.Context().Done() })
	_, err = callImages(ctx, newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "p", N: 1})
	if ce, ok := err.(*CallError); !ok || !ce.Billed || !strings.Contains(ce.Message, "timed out") {
		t.Fatalf("a timeout should read as one and may be billed: %#v", err)
	}
}

func TestRedirectsToAnotherHostAreNotFollowed(t *testing.T) {
	other := newFakeAPI(t)
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) {
		http.Redirect(w, r, other.srv.URL+"/v1/images/generations", http.StatusTemporaryRedirect)
	})
	_, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "p", N: 1})
	if err == nil || other.count() != 0 {
		t.Fatalf("the key was sent on to another host (calls %d, err %v)", other.count(), err)
	}
}

func TestEndpointPrintsWithoutItsKey(t *testing.T) {
	e := Endpoint{BaseURL: "https://api.example.test/v1", Key: testKey}
	for _, s := range []string{fmt.Sprint(e), fmt.Sprintf("%v", e), fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e), fmt.Sprintf("%s", e), fmt.Sprint(&e)} {
		if strings.Contains(s, "SECRET") {
			t.Errorf("printed the key: %q", s)
		}
	}
}

func TestAnAnswerWithoutUsageSaysSo(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) { api.respond(w, 1, false) })
	reply, err := callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "p", N: 1})
	if err != nil || reply.HasUsage {
		t.Fatalf("reply = %+v (%v)", reply, err)
	}
	api.setHandler(func(w http.ResponseWriter, r *http.Request, c call) {
		fmt.Fprint(w, `{"data":[{"b64_json":"AAAA"}],"usage":{"input_tokens":50,"output_tokens":10}}`)
	})
	reply, err = callImages(context.Background(), newClient(), api.endpoint(), imageCall{Model: "m", Prompt: "p", N: 1})
	if err != nil || reply.Usage != (Usage{ImageIn: 50, ImageOut: 10}) {
		t.Fatalf("an unsplit input count should be priced as image input, got %+v (%v)", reply.Usage, err)
	}
}

func TestOpenRouterReportedCostIsRecorded(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.Provider = "openrouter" })
	ref, err := r.m.Store().AddRef(discPNG(t, 20, 20, color.NRGBA{255, 0, 0, 255}))
	if err != nil {
		t.Fatal(err)
	}
	out := discPNG(t, 16, 16, color.NRGBA{0, 255, 0, 255})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			http.NotFound(w, req)
			return
		}
		fmt.Fprintf(w, `{"data":[{"b64_json":"%s"}],"usage":{"cost":0.0123}}`, base64.StdEncoding.EncodeToString(out))
	}))
	t.Cleanup(server.Close)
	r.endpoint = func(string) (Endpoint, error) { return Endpoint{BaseURL: server.URL + "/v1", Key: testKey}, nil }
	j := r.finish(r.generate(GenerateInput{Prompt: "p", Refs: []string{ref.ID}, Count: 1}).ID)
	if j.State != StateReady || j.CostUSD == nil || *j.CostUSD != 0.0123 || j.CostFromEstimate {
		t.Fatalf("job=%+v", j)
	}
}

func TestReferencePreflightRefusesListedModelWithoutInputReferences(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			fmt.Fprint(w, liveListing("text-only", descriptorsFor("size", "quality")))
			return
		}
		posts++
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()
	_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL + "/v1", Key: testKey, Provider: "openrouter"}, imageCall{Model: "text-only", Prompt: "p", N: 1, Refs: [][]byte{[]byte("ref")}})
	ce, ok := err.(*CallError)
	if !ok || posts != 0 || ce.Billed || !strings.Contains(ce.Message, "input_references") {
		t.Fatalf("error=%#v posts=%d", err, posts)
	}
}

func TestReferencePreflightProceedsWhenListingFailsOrModelIsUnlisted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		list   string
	}{
		{"listing fails", http.StatusInternalServerError, ""},
		{"model unlisted", http.StatusOK, liveListing("other", descriptorsFor("size"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.list)
					return
				}
				posts++
				fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
			}))
			defer server.Close()
			_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "m", Prompt: "p", N: 1, Refs: [][]byte{[]byte("ref")}})
			if err != nil || posts != 1 {
				t.Fatalf("error=%v posts=%d", err, posts)
			}
		})
	}
}

func TestOpenRouterStatusesHaveDistinctMessages(t *testing.T) {
	seen := map[string]bool{}
	for _, status := range []int{400, 401, 402, 404, 429, 502} {
		body := `{"error":{"message":"detail data:image/png;base64,QUJDREVG ` + testKey + `"}}`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}))
		_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Key: testKey, Provider: "openrouter"}, imageCall{Model: "m", Prompt: "p", N: 1, Refs: [][]byte{[]byte("ref")}})
		server.Close()
		ce, ok := err.(*CallError)
		if !ok || ce.Status != status || !strings.Contains(ce.Message, fmt.Sprintf("(HTTP %d)", status)) {
			t.Fatalf("status %d: error=%#v", status, err)
		}
		for _, leak := range []string{testKey, "QUJDREVG", "data:image", "base64"} {
			if strings.Contains(ce.Message, leak) {
				t.Errorf("status %d: error message leaks %q", status, leak)
			}
		}
		if seen[ce.Message] {
			t.Errorf("status %d: message repeats another status's message", status)
		}
		seen[ce.Message] = true
	}
}

func TestOpenRouterTextPreflightAndMultiplePictures(t *testing.T) {
	var gets, posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets++
			if r.URL.Path != "/v1/images/models" {
				t.Error("wrong model listing path")
			}
			fmt.Fprint(w, liveListing("m", "{}"))
			return
		}
		posts++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images" {
			t.Error("wrong image endpoint")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid JSON request")
			return
		}
		for _, field := range []string{"n", "output_format", "input_references", "size", "quality"} {
			if _, ok := body[field]; ok {
				t.Errorf("unexpected field %s", field)
			}
		}
		fmt.Fprint(w, `{"data":[{"b64_json":"YQ==","media_type":"image/png"}],"usage":{"cost":0.1}}`)
	}))
	defer server.Close()
	ep := Endpoint{BaseURL: server.URL + "/v1", Provider: "openrouter"}
	c := imageCall{Model: "m", Prompt: "p", N: 2, Size: "1024x1024", Quality: "medium", defaults: map[string]bool{"size": true, "quality": true}}
	for i := 0; i < 2; i++ {
		r, err := callImages(context.Background(), newClient(), ep, c)
		if err != nil || len(r.Images) != 2 || !r.HasCost || r.CostUSD != 0.2 {
			t.Fatalf("unexpected reply or error: %v", err)
		}
	}
	if gets != 1 || posts != 4 {
		t.Fatalf("gets=%d posts=%d", gets, posts)
	}
}

func TestOpenRouterPreflightRejectsUnsupportedParameters(t *testing.T) {
	for _, field := range []string{"input_references", "size", "quality", "background"} {
		t.Run(field, func(t *testing.T) {
			var posts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprint(w, liveListing("m", "{}"))
					return
				}
				posts++
			}))
			defer server.Close()
			c := imageCall{Model: "m", N: 1}
			switch field {
			case "input_references":
				c.Refs = [][]byte{[]byte("ref")}
			case "size":
				c.Size = "1024x1024"
			case "quality":
				c.Quality = "high"
			case "background":
				c.Background = "transparent"
			}
			_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, c)
			if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "Stop and report") || strings.Contains(err.Error(), "without") || posts != 0 {
				t.Fatalf("error=%v posts=%d", err, posts)
			}
		})
	}
}

func TestOpenRouterRecoveredJobPreservesResolvedSettings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, liveListing("m", "{}"))
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
			return
		}
		if body["size"] != "1024x1024" || body["quality"] != "medium" {
			t.Error("recovered settings were omitted")
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "m", Size: "1024x1024", Quality: "medium", recovered: true})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenRouterSupportedOptionalParameters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, liveListing("m", descriptorsFor("size", "quality", "background", "n", "output_format")))
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
			return
		}
		for k, want := range map[string]any{"size": "1024x1024", "quality": "high", "background": "transparent", "n": float64(1), "output_format": "png"} {
			if body[k] != want {
				t.Errorf("unexpected %s", k)
			}
		}
		fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
	}))
	defer server.Close()
	_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "m", Size: "1024x1024", Quality: "high", Background: "transparent", N: 1})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenRouterTimeoutAndRedaction(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		<-release
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := callImages(ctx, newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter", Key: testKey}, imageCall{N: 1})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected timeout error: %v", err)
	}
	for _, body := range []string{`{"error":{"message":"QUJDREVG ` + testKey + `"}}`, `{"data":[{"b64_json":"QUJDREVG"}],`, `{"data":[],"payload":"QUJDREVG"}`} {
		_, err := decodeRefReply([]byte(body), 200)
		if err == nil || strings.Contains(err.Error(), "QUJDREVG") || strings.Contains(err.Error(), testKey) {
			t.Error("unsafe or missing decode error")
		}
		ce := openRouterStatusError(400, []byte(body), testKey)
		if strings.Contains(ce.Message, "QUJDREVG") || strings.Contains(ce.Message, testKey) {
			t.Error("unsafe status error")
		}
	}
	ce := openRouterStatusError(400, []byte(`{"error":{"message":"input_references not supported; remove references data:image/png;base64,QUJDREVG `+testKey+`"}}`), testKey)
	if !strings.Contains(ce.Message, "input_references is unsupported") || strings.Contains(ce.Message, "remove references") || strings.Contains(ce.Message, "QUJDREVG") || strings.Contains(ce.Message, testKey) {
		t.Error("unsafe or missing reference diagnostic")
	}
}

// A model list fetched with a key the provider has since refused must not be trusted: fixing the key has to lead to a
// capability check again, not to a POST sent on the old listing.
func TestOpenRouterPreflightIsRepeatedAfterAFixedKey(t *testing.T) {
	var (
		mu    sync.Mutex
		auth  = "test-key-revoked"
		asked int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		want := "Bearer " + auth
		mu.Unlock()
		if r.Method == http.MethodGet {
			asked++
			if r.Header.Get("Authorization") != want {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(401)
	}))
	defer server.Close()
	clearModelListing(server.URL)
	ep := func() Endpoint {
		mu.Lock()
		defer mu.Unlock()
		return Endpoint{BaseURL: server.URL, Provider: "openrouter", Key: auth}
	}
	c := imageCall{Model: "m", Prompt: "p"}
	_, err := callImages(context.Background(), newClient(), ep(), c)
	if err == nil {
		t.Fatal("revoked key was not reported")
	}
	mu.Lock()
	auth = "test-key-working"
	mu.Unlock()
	_, err = callImages(context.Background(), newClient(), ep(), c)
	if err == nil {
		t.Fatal("second call did not reach the POST")
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 2 {
		t.Fatalf("model list asked %d times, want 2: the fixed key was never checked", asked)
	}
}

func TestOpenRouterUnlistedModelSendsOnlyTheChoiceFields(t *testing.T) {
	png := base64.StdEncoding.EncodeToString(discPNG(t, 16, 16, color.NRGBA{0, 0, 0, 0}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
			return
		}
		for _, k := range []string{"n", "output_format", "input_references"} {
			if _, ok := body[k]; ok {
				t.Errorf("%s was sent to a model whose parameters are unknown", k)
			}
		}
		for _, k := range []string{"size", "quality", "background"} {
			if body[k] == nil {
				t.Errorf("%s was dropped for a model whose parameters are unknown", k)
			}
		}
		fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, png)
	}))
	defer server.Close()
	clearModelListing(server.URL)
	c := imageCall{Model: "m", Prompt: "p", N: 2, Size: "1024x1024", Quality: "high", Background: "transparent"}
	if _, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, c); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRouterManagerDefaultsAndExplicitParameters(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.Provider = "openrouter" })
	out := discPNG(t, 16, 16, color.NRGBA{0, 255, 0, 255})
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			fmt.Fprint(w, liveListing(DefaultModel, "{}"))
			return
		}
		posts++
		var body map[string]any
		if json.NewDecoder(req.Body).Decode(&body) != nil {
			t.Error("invalid request")
			return
		}
		if _, ok := body["size"]; ok {
			t.Error("default size sent")
		}
		if _, ok := body["quality"]; ok {
			t.Error("default quality sent")
		}
		fmt.Fprintf(w, `{"data":[{"b64_json":"%s"}]}`, base64.StdEncoding.EncodeToString(out))
	}))
	t.Cleanup(server.Close)
	r.endpoint = func(string) (Endpoint, error) { return Endpoint{BaseURL: server.URL, Provider: "openrouter"}, nil }
	if j := r.finish(r.generate(GenerateInput{Count: 1}).ID); j.State != StateReady {
		t.Fatalf("default job state=%s message=%s", j.State, j.Message)
	}
	if j := r.finish(r.generate(GenerateInput{Count: 1, Size: "1536x1024"}).ID); j.State != StateFailed || !strings.Contains(j.Message, "size") {
		t.Fatalf("explicit job state=%s message=%s", j.State, j.Message)
	}
	if posts != 1 {
		t.Fatalf("posts=%d", posts)
	}
}

// Recorded in UnknownHealth jobs j30..j75: the playbook tells agents to pass size 1024x1024, the user's default is
// 1024x1024, and the model lists quality and background but not size. Every such job was refused with "does not support
// requested parameter size" and told to stop. A value equal to the user's default overrides nothing, so it is dropped
// like the default; a different explicit size is still refused.
func TestOpenRouterExplicitSettingEqualToDefaultIsDroppedLikeTheDefault(t *testing.T) {
	out := discPNG(t, 16, 16, color.NRGBA{0, 255, 0, 255})
	for _, tc := range []struct {
		name  string
		in    GenerateInput
		ready bool
	}{
		{"size equal to default", GenerateInput{Count: 1, Size: "1024x1024"}, true},
		{"size and quality equal to default", GenerateInput{Count: 1, Size: "1024x1024", Quality: "Medium"}, true},
		{"size different from default", GenerateInput{Count: 1, Size: "1536x1024"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(s *Settings) { s.Provider = "openrouter" })
			var posts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					fmt.Fprint(w, liveListing(DefaultModel, `{"background":{"type":"enum","values":["auto","opaque"]}}`))
					return
				}
				posts++
				var body map[string]any
				if json.NewDecoder(req.Body).Decode(&body) != nil {
					t.Error("invalid request")
					return
				}
				for _, name := range []string{"size", "quality"} {
					if _, ok := body[name]; ok {
						t.Errorf("%s sent although the model does not list it", name)
					}
				}
				fmt.Fprintf(w, `{"data":[{"b64_json":"%s"}]}`, base64.StdEncoding.EncodeToString(out))
			}))
			t.Cleanup(server.Close)
			r.endpoint = func(string) (Endpoint, error) { return Endpoint{BaseURL: server.URL, Provider: "openrouter"}, nil }
			j := r.finish(r.generate(tc.in).ID)
			if tc.ready && (j.State != StateReady || posts != 1) {
				t.Fatalf("state=%s posts=%d message=%s", j.State, posts, j.Message)
			}
			if !tc.ready && (j.State != StateFailed || !strings.Contains(j.Message, "parameter size") || posts != 0) {
				t.Fatalf("state=%s posts=%d message=%s", j.State, posts, j.Message)
			}
		})
	}
}

// mixedListing is GET /images/models in the live shape, trimmed to the descriptor types seen on 2026-10-28, plus one
// malformed model entry, one empty id and one descriptor whose fields do not decode.
const mixedListing = `{"data":[
 {"id":"good","supported_parameters":{
   "aspect_ratio":{"type":"enum","values":["1:1","16:9"]},
   "input_references":{"type":"range","min":0,"max":2},
   "n":{"type":"range","min":1,"max":6},
   "seed":{"type":"boolean","min":"not a number"},
   "future":{"type":"something_new"}}},
 {"id":"bad","supported_parameters":["size"]},
 {"id":"","supported_parameters":{}}
]}`

func TestLiveListingDecodesDescriptorsPerModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, mixedListing)
	}))
	defer server.Close()
	params, err := fetchModelParams(context.Background(), newClient(), Endpoint{BaseURL: server.URL}, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := params["bad"]; ok || len(params) != 1 {
		t.Fatalf("malformed or empty-id model kept: %v", params)
	}
	good := params["good"]
	if !rangeAccepts(good["input_references"], 2) || rangeAccepts(good["input_references"], 3) {
		t.Errorf("input_references range=%+v", good["input_references"])
	}
	if len(good["aspect_ratio"].Values) != 2 || good["aspect_ratio"].Type != "enum" {
		t.Errorf("aspect_ratio=%+v", good["aspect_ratio"])
	}
	if d, ok := good["seed"]; !ok || d.Type != "" {
		t.Errorf("malformed descriptor should keep its name without limits: %+v ok=%v", d, ok)
	}
	if good["future"].Type != "something_new" {
		t.Errorf("unknown type not kept: %+v", good["future"])
	}
	if got := strings.Join(paramNames(good), ","); got != "aspect_ratio,future,input_references,n,seed" {
		t.Errorf("status names=%s", got)
	}
}

func TestMalformedModelDoesNotDiscardListingOrCache(t *testing.T) {
	var gets, posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets++
			fmt.Fprint(w, mixedListing)
			return
		}
		posts++
		fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
	}))
	defer server.Close()
	ep := Endpoint{BaseURL: server.URL, Provider: "openrouter"}
	// The good model's input_references max is 2: three references are refused from the cached listing.
	for i := 0; i < 2; i++ {
		_, err := callImages(context.Background(), newClient(), ep, imageCall{Model: "good", N: 1, Refs: [][]byte{[]byte("a"), []byte("b"), []byte("c")}})
		if err == nil || !strings.Contains(err.Error(), "accepts 0 to 2 input_references, but this job has 3") || posts != 0 {
			t.Fatalf("err=%v posts=%d", err, posts)
		}
	}
	if gets != 1 {
		t.Fatalf("listing fetched %d times, want 1", gets)
	}
	// A model whose entry was malformed is unlisted, so the job proceeds as with any unlisted model.
	if _, err := callImages(context.Background(), newClient(), ep, imageCall{Model: "bad", N: 1, Refs: [][]byte{[]byte("a")}}); err != nil || posts != 1 {
		t.Fatalf("unlisted model err=%v posts=%d", err, posts)
	}
}

func TestReferenceCountRefusedBeforePostAboveRangeMax(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs int
		ok   bool
	}{{"at max", 2, true}, {"over max", 3, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var posts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprint(w, mixedListing)
					return
				}
				posts++
				fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
			}))
			defer server.Close()
			refs := make([][]byte, tc.refs)
			for i := range refs {
				refs[i] = []byte("ref")
			}
			_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "good", N: 1, Refs: refs})
			if tc.ok != (err == nil) || (tc.ok && posts != 1) || (!tc.ok && posts != 0) {
				t.Fatalf("err=%v posts=%d", err, posts)
			}
			if !tc.ok && (!strings.Contains(err.Error(), "Stop and report") || strings.Contains(err.Error(), "data:")) {
				t.Fatalf("message=%v", err)
			}
		})
	}
}

func TestNSentOnlyWithinListedRangeElsePerPicture(t *testing.T) {
	for _, tc := range []struct {
		name     string
		listing  string
		n        int
		wantPost int
		wantN    bool
	}{
		{"listed range holds count", liveListing("m", `{"n":{"type":"range","min":1,"max":6}}`), 3, 1, true},
		{"count above listed max", liveListing("m", `{"n":{"type":"range","min":1,"max":2}}`), 3, 3, false},
		{"n not listed", liveListing("m", `{"seed":{"type":"boolean"}}`), 2, 2, false},
		{"unknown n descriptor", liveListing("m", `{"n":{"type":"something_new"}}`), 2, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprint(w, tc.listing)
					return
				}
				posts++
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid request")
					return
				}
				_, hasN := body["n"]
				if hasN != tc.wantN || (tc.wantN && body["n"] != float64(tc.n)) {
					t.Errorf("n sent=%v value=%v", hasN, body["n"])
				}
				pictures := 1
				if hasN {
					pictures = tc.n
				}
				items := make([]string, pictures)
				for i := range items {
					items[i] = `{"b64_json":"YQ=="}`
				}
				fmt.Fprintf(w, `{"data":[%s],"usage":{"cost":0.01}}`, strings.Join(items, ","))
			}))
			defer server.Close()
			r, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "m", N: tc.n})
			if err != nil || posts != tc.wantPost || len(r.Images) != tc.n {
				t.Fatalf("err=%v posts=%d images=%d", err, posts, len(r.Images))
			}
		})
	}
}

func TestListedEnumValuesRefusedWhenAgentSetAndDroppedWhenInherited(t *testing.T) {
	const listing = `{"data":[{"id":"m","supported_parameters":{"quality":{"type":"enum","values":["low","medium"]},"size":{"type":"boolean"}}}]}`
	t.Run("agent-set value outside list refused", func(t *testing.T) {
		var posts int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				fmt.Fprint(w, listing)
				return
			}
			posts++
		}))
		defer server.Close()
		_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "m", N: 1, Quality: "high"})
		if err == nil || !strings.Contains(err.Error(), "quality") || !strings.Contains(err.Error(), "Accepted values: low, medium") || posts != 0 {
			t.Fatalf("err=%v posts=%d", err, posts)
		}
	})
	t.Run("inherited default outside list dropped", func(t *testing.T) {
		var posts int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				fmt.Fprint(w, listing)
				return
			}
			posts++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["quality"]; ok {
				t.Error("inherited quality sent")
			}
			if body["size"] != "1024x1024" {
				t.Errorf("listed boolean size dropped: %v", body["size"])
			}
			fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
		}))
		defer server.Close()
		c := imageCall{Model: "m", N: 1, Quality: "high", Size: "1024x1024", defaults: map[string]bool{"quality": true, "size": true}}
		if _, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, c); err != nil || posts != 1 {
			t.Fatalf("err=%v posts=%d", err, posts)
		}
	})
}

// Recorded in UnknownHealth job j104 (and j102, j103, j84): model openai/gpt-image-2.5-sunburst, one reference, quality
// medium, background opaque, count 1, the user's size default inherited. The server took about 20 s, then answered 400,
// and Relo told the agent only "rejected the request as invalid (HTTP 400)", so the agent reworded and resent the job
// twice more. The live listing for that model (GET /api/v1/images/models, 2026-10-28) names aspect_ratio, quality,
// background, n, input_references and output_compression, and no size. The real 400 body was never kept; the body here is
// OpenRouter's documented envelope for an upstream refusal, with a moderation_blocked code as the upstream's reason.
func TestOpenRouterUpstream400ReasonReachesTheAgentInFixedWords(t *testing.T) {
	const model = "openai/gpt-image-2.5-sunburst"
	const listing = `{"aspect_ratio":{"type":"enum","values":["1:1","3:2","2:3","4:3","3:4","16:9","9:16","21:9","auto"]},` +
		`"quality":{"type":"enum","values":["auto","low","medium","high","xhigh","max"]},` +
		`"background":{"type":"enum","values":["auto","transparent","opaque"]},` +
		`"n":{"type":"range","min":1,"max":10},"input_references":{"type":"range","min":0,"max":16},` +
		`"output_compression":{"type":"range","min":0,"max":100}}`
	upstream := `{"error":{"message":"Your request was rejected by the safety system. safety_violations=[violence] data:image/png;base64,QUJDREVG ` + testKey + `","type":"image_generation_user_error","code":"moderation_blocked"}}`
	envelope, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": 400, "message": "Provider returned error",
		"metadata": map[string]any{"raw": upstream, "provider_name": "OpenAI"},
	}})
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, liveListing(model, listing))
			return
		}
		if json.NewDecoder(r.Body).Decode(&posted) != nil {
			t.Error("invalid request")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(envelope)
	}))
	defer server.Close()
	ref := discPNG(t, 16, 16, color.NRGBA{0, 160, 0, 255})
	_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Key: testKey, Provider: "openrouter"}, imageCall{
		Model: model, Prompt: "Match the look of image 1.", Refs: [][]byte{ref}, N: 1,
		Size: "1024x1024", Quality: "medium", Background: "opaque", defaults: map[string]bool{"size": true},
	})
	ce, ok := err.(*CallError)
	if !ok || ce.Status != 400 || ce.Billed {
		t.Fatalf("error = %#v", err)
	}
	// The request itself is well formed: the inherited size is dropped, the listed settings and the reference go out.
	refs, _ := posted["input_references"].([]any)
	if _, hasSize := posted["size"]; hasSize || posted["quality"] != "medium" || posted["background"] != "opaque" || posted["model"] != model || len(refs) != 1 {
		t.Fatalf("request body = %v", posted)
	}
	for _, want := range []string{"HTTP 400", "safety filter refused the prompt or a reference picture", "Provider code: moderation_blocked.", "Provider: OpenAI.", "Stop and report this to the user"} {
		if !strings.Contains(ce.Message, want) {
			t.Errorf("message lacks %q: %s", want, ce.Message)
		}
	}
	for _, leak := range []string{testKey, "QUJDREVG", "data:image", "base64", "violence", "rejected by the safety system"} {
		if strings.Contains(ce.Message, leak) {
			t.Errorf("message leaks provider wording %q: %s", leak, ce.Message)
		}
	}
}

func TestOpenRouterRefusalClassesAndUnsafeCodes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
		notWant    []string
	}{
		{"plain message, no class", `{"error":{"message":"bad things"}}`, nil, []string{"Provider code", "Provider:", "safety", "could not read", "too long"}},
		{"unreadable reference", `{"error":{"message":"Provider returned error","metadata":{"raw":"{\"error\":{\"code\":\"invalid_image_format\",\"message\":\"Invalid image file or mode for image 1\"}}","provider_name":"OpenAI"}}}`,
			[]string{"could not read a reference picture", "Provider code: invalid_image_format."}, []string{"safety filter"}},
		{"prompt too long", `{"error":{"message":"Provider returned error","metadata":{"raw":"{\"error\":{\"code\":\"string_above_max_length\"}}"}}}`,
			[]string{"prompt is too long", "Provider code: string_above_max_length."}, nil},
		{"numeric and hostile codes are not repeated", `{"error":{"code":400,"type":"Sk-Not A Code!","message":"x","metadata":{"provider_name":"<script>alert(1)</script>"}}}`,
			nil, []string{"Provider code", "Provider:", "script", "Sk-Not"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ce := openRouterStatusError(400, []byte(tc.body), "")
			for _, w := range tc.want {
				if !strings.Contains(ce.Message, w) {
					t.Errorf("message lacks %q: %s", w, ce.Message)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(ce.Message, w) {
					t.Errorf("message has %q: %s", w, ce.Message)
				}
			}
		})
	}
	// A refusal class is only reported for the statuses that carry a request refusal.
	if m := openRouterStatusError(401, []byte(`{"error":{"message":"moderation"}}`), "").Message; strings.Contains(m, "safety filter") {
		t.Errorf("401 message = %s", m)
	}
}

func TestOpenRouterUnexpectedPictureCountSaysHowManyCameBack(t *testing.T) {
	out := discPNG(t, 16, 16, color.NRGBA{0, 255, 0, 255})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, liveListing("m", descriptorsFor("n", "input_references")))
			return
		}
		fmt.Fprintf(w, `{"data":[{"b64_json":"%s"}]}`, base64.StdEncoding.EncodeToString(out))
	}))
	defer server.Close()
	_, err := callImages(context.Background(), newClient(), Endpoint{BaseURL: server.URL, Provider: "openrouter"}, imageCall{Model: "m", Prompt: "p", N: 2, Refs: [][]byte{[]byte("ref")}})
	ce, ok := err.(*CallError)
	if !ok || !ce.Billed || !strings.Contains(ce.Message, "unexpected number of pictures for one request (asked for 2, got 1)") {
		t.Fatalf("error = %#v", err)
	}
}
