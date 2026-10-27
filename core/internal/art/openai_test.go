package art

import (
	"context"
	"fmt"
	"image/color"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
