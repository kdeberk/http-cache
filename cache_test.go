package cache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

type adapterMock struct {
	sync.Mutex
	store map[uint64][]byte
}

type errReader int

func (a *adapterMock) Get(key uint64) ([]byte, bool) {
	a.Lock()
	defer a.Unlock()
	if _, ok := a.store[key]; ok {
		return a.store[key], true
	}
	return nil, false
}

func (a *adapterMock) Set(key uint64, response []byte, expiration time.Time) {
	a.Lock()
	defer a.Unlock()
	a.store[key] = response
}

func (a *adapterMock) Release(key uint64) {
	a.Lock()
	defer a.Unlock()
	delete(a.store, key)
}

func (errReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("readAll error")
}

func TestMiddleware(t *testing.T) {
	type req struct {
		url      string
		method   string
		body     []byte
		wantBody string
	}

	tests := []struct {
		name string
		reqs []req
	}{
		{
			name: "identical requests receive same, cached response",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 1"},
			},
		},
		{
			name: "different url receives different response",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 1"},
				{url: "http://foo.bar/test-2", method: "GET", body: nil, wantBody: "cached value 2"},
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 1"},
			},
		},
		{
			name: "same url, GET and POST requests receives same response",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1", method: "POST", body: nil, wantBody: "cached value 1"},
			},
		},
		{
			name: "PUT responses are not cached",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "PUT", body: nil, wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1", method: "PUT", body: nil, wantBody: "cached value 2"},
				{url: "http://foo.bar/test-1", method: "PUT", body: nil, wantBody: "cached value 3"},
			},
		},
		{
			name: "same url, GET and POST with body receive different response",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar"}`), wantBody: "cached value 2"},
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar"}`), wantBody: "cached value 2"},
			},
		},
		{
			name: "POSTs with different bodies generate different responses",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar1"}`), wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar2"}`), wantBody: "cached value 2"},
			},
		},
		{
			name: "GET body is ignored",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar"}`), wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1", method: "GET", body: []byte(`{"foo": "bar"}`), wantBody: "cached value 2"},
			},
		},
		{
			name: "refresh key resets the cache for GET",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1?rk=true", method: "GET", body: nil, wantBody: "cached value 2"},
				{url: "http://foo.bar/test-1?rk=true", method: "GET", body: nil, wantBody: "cached value 3"},
				{url: "http://foo.bar/test-1", method: "GET", body: nil, wantBody: "cached value 3"},
			},
		},
		{
			name: "refresh key resets the cache for POST with that body",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar"}`), wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1?rk=true", method: "POST", body: []byte(`{"foo": "bar"}`), wantBody: "cached value 2"},
				{url: "http://foo.bar/test-1?rk=true", method: "POST", body: []byte(`{"foo": "bar"}`), wantBody: "cached value 3"},
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar"}`), wantBody: "cached value 3"}, // TODO: possible bug
			},
		},
		{
			name: "refresh key resets the cache for POST with that specific body",
			reqs: []req{
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar1"}`), wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar2"}`), wantBody: "cached value 2"},
				{url: "http://foo.bar/test-1?rk=true", method: "POST", body: []byte(`{"foo": "bar1"}`), wantBody: "cached value 3"},
				{url: "http://foo.bar/test-1", method: "POST", body: []byte(`{"foo": "bar2"}`), wantBody: "cached value 2"},
			},
		},
		{
			name: "uses query params to generate key",
			reqs: []req{
				{url: "http://foo.bar/test-1?foo=1", method: "GET", wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1?foo=2", method: "GET", wantBody: "cached value 2"},
				{url: "http://foo.bar/test-1?bar=1&foo=2", method: "GET", wantBody: "cached value 3"},
			},
		},
		{
			name: "orders query params before generating key",
			reqs: []req{
				{url: "http://foo.bar/test-1?foo=1&bar=2", method: "GET", wantBody: "cached value 1"},
				{url: "http://foo.bar/test-1?bar=1&foo=2", method: "GET", wantBody: "cached value 2"},
				{url: "http://foo.bar/test-1?bar=2&foo=1", method: "GET", wantBody: "cached value 1"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			client, _ := NewClient(
				ClientWithAdapter(&adapterMock{
					store: map[uint64][]byte{},
				}),
				ClientWithTTL(1*time.Minute),
				ClientWithRefreshKey("rk"),
				ClientWithMethods([]string{http.MethodGet, http.MethodPost}),
			)

			counter := 0
			httpTestHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				counter++
				w.Write([]byte(fmt.Sprintf("cached value %v", counter)))
			})

			handler := client.Middleware(httpTestHandler)

			for idx, r := range tt.reqs {
				var reader io.Reader
				if r.body != nil {
					reader = bytes.NewReader(r.body)
				}
				req, err := http.NewRequest(r.method, r.url, reader)
				if err != nil {
					t.Fatalf("failed to create request: %v", err)
				}

				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)

				resp := w.Result()
				bs, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("failed to read body: %v", err)
				}
				if string(bs) != r.wantBody {
					t.Errorf("[%d] *Client.Middleware() = %s, want %v", idx, string(bs), r.wantBody)
				}
			}
		})
	}
}

func TestBytesToResponse(t *testing.T) {
	r := Response{
		Value:      []byte("value 1"),
		Expiration: time.Time{},
		Frequency:  0,
		LastAccess: time.Time{},
	}

	tests := []struct {
		name      string
		b         []byte
		wantValue string
	}{

		{
			"convert bytes array to response",
			r.Bytes(),
			"value 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BytesToResponse(tt.b)
			if string(got.Value) != tt.wantValue {
				t.Errorf("BytesToResponse() Value = %v, want %v", got, tt.wantValue)
				return
			}
		})
	}
}

func TestResponseToBytes(t *testing.T) {
	r := Response{
		Value:      nil,
		Expiration: time.Time{},
		Frequency:  0,
		LastAccess: time.Time{},
	}

	tests := []struct {
		name     string
		response Response
	}{
		{
			"convert response to bytes array",
			r,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.response.Bytes()
			if b == nil || len(b) == 0 {
				t.Error("Bytes() failed to convert")
				return
			}
		})
	}
}

func TestSortURLParams(t *testing.T) {
	u, _ := url.Parse("http://test.com?zaz=bar&foo=zaz&boo=foo&boo=baz")
	tests := []struct {
		name string
		URL  *url.URL
		want string
	}{
		{
			"returns url with ordered querystring params",
			u,
			"http://test.com?boo=baz&boo=foo&foo=zaz&zaz=bar",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sortURLParams(tt.URL)
			got := tt.URL.String()
			if got != tt.want {
				t.Errorf("sortURLParams() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGenerateKeyString(t *testing.T) {
	urls := []string{
		"http://localhost:8080/category",
		"http://localhost:8080/category/morisco",
		"http://localhost:8080/category/mourisquinho",
	}

	keys := make(map[string]string, len(urls))
	for _, u := range urls {
		rawKey := generateKey(u)
		key := KeyAsString(rawKey)

		if otherURL, found := keys[key]; found {
			t.Fatalf("URLs %s and %s share the same key %s", u, otherURL, key)
		}
		keys[key] = u
	}
}

func TestGenerateKey(t *testing.T) {
	tests := []struct {
		name string
		URL  string
		want uint64
	}{
		{
			"get url checksum",
			"http://foo.bar/test-1",
			14974843192121052621,
		},
		{
			"get url 2 checksum",
			"http://foo.bar/test-2",
			14974839893586167988,
		},
		{
			"get url 3 checksum",
			"http://foo.bar/test-3",
			14974840993097796199,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := generateKey(tt.URL); got != tt.want {
				t.Errorf("generateKey() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGenerateKeyWithBody(t *testing.T) {
	tests := []struct {
		name string
		URL  string
		body []byte
		want uint64
	}{
		{
			"get POST checksum",
			"http://foo.bar/test-1",
			[]byte(`{"foo": "bar"}`),
			16224051135567554746,
		},
		{
			"get POST 2 checksum",
			"http://foo.bar/test-1",
			[]byte(`{"bar": "foo"}`),
			3604153880186288164,
		},
		{
			"get POST 3 checksum",
			"http://foo.bar/test-2",
			[]byte(`{"foo": "bar"}`),
			10956846073361780255,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := generateKeyWithBody(tt.URL, tt.body); got != tt.want {
				t.Errorf("generateKeyWithBody() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	adapter := &adapterMock{}

	tests := []struct {
		name    string
		opts    []ClientOption
		want    *Client
		wantErr bool
	}{
		{
			"returns new client",
			[]ClientOption{
				ClientWithAdapter(adapter),
				ClientWithTTL(1 * time.Millisecond),
				ClientWithMethods([]string{http.MethodGet, http.MethodPost}),
			},
			&Client{
				adapter:    adapter,
				ttl:        1 * time.Millisecond,
				refreshKey: "",
				methods:    []string{http.MethodGet, http.MethodPost},
			},
			false,
		},
		{
			"returns new client with refresh key",
			[]ClientOption{
				ClientWithAdapter(adapter),
				ClientWithTTL(1 * time.Millisecond),
				ClientWithRefreshKey("rk"),
			},
			&Client{
				adapter:    adapter,
				ttl:        1 * time.Millisecond,
				refreshKey: "rk",
				methods:    []string{http.MethodGet},
			},
			false,
		},
		{
			"returns error",
			[]ClientOption{
				ClientWithAdapter(adapter),
			},
			nil,
			true,
		},
		{
			"returns error",
			[]ClientOption{
				ClientWithTTL(1 * time.Millisecond),
				ClientWithRefreshKey("rk"),
			},
			nil,
			true,
		},
		{
			"returns error",
			[]ClientOption{
				ClientWithAdapter(adapter),
				ClientWithTTL(0),
				ClientWithRefreshKey("rk"),
			},
			nil,
			true,
		},
		{
			"returns error",
			[]ClientOption{
				ClientWithAdapter(adapter),
				ClientWithTTL(1 * time.Millisecond),
				ClientWithMethods([]string{http.MethodGet, http.MethodPut}),
			},
			nil,
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewClient(tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewClient() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewClient() = %v, want %v", got, tt.want)
			}
		})
	}
}
