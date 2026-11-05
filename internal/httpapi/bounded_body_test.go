package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeBoundedJSON(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	const limit = 32
	valid := `{"name":"task"}`
	for _, tc := range []struct {
		name    string
		body    io.Reader
		length  int64
		status  int
		message string
	}{
		{name: "within the bound", body: strings.NewReader(valid), length: int64(len(valid)), status: http.StatusOK},
		{name: "trailing whitespace within the bound", body: strings.NewReader(valid + "  \n"), length: int64(len(valid) + 3), status: http.StatusOK},
		{name: "declared length over the bound", body: strings.NewReader(strings.Repeat(" ", limit+1)), length: limit + 1, status: http.StatusRequestEntityTooLarge, message: "request body exceeds the size limit"},
		{name: "chunked body over the bound", body: io.MultiReader(strings.NewReader(valid), strings.NewReader(strings.Repeat(" ", limit))), length: -1, status: http.StatusRequestEntityTooLarge, message: "request body exceeds the size limit"},
		{name: "malformed", body: strings.NewReader(`{"name":`), length: 8, status: http.StatusBadRequest, message: "invalid JSON body"},
		{name: "trailing data", body: strings.NewReader(valid + `{}`), length: int64(len(valid) + 2), status: http.StatusBadRequest, message: "invalid JSON body"},
		{name: "empty", body: strings.NewReader(""), length: 0, status: http.StatusBadRequest, message: "invalid JSON body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/example", tc.body)
			request.ContentLength = tc.length
			response := httptest.NewRecorder()
			var decoded payload
			err := decodeBoundedJSON(response, request, limit, &decoded)
			if tc.status == http.StatusOK {
				if err != nil || decoded.Name != "task" {
					t.Fatalf("decoded=%+v err=%v", decoded, err)
				}
				return
			}
			if err == nil {
				t.Fatal("decodeBoundedJSON accepted the body")
			}
			writeBodyDecodeError(response, err)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.message) {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestDecodeBoundedJSONKeepsItsReadErrorOutOfTheBound(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/example", io.NopCloser(failingBodyReader{}))
	request.ContentLength = -1
	var decoded json.RawMessage
	err := decodeBoundedJSON(httptest.NewRecorder(), request, 32, &decoded)
	if err == nil || errors.Is(err, errBodyTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

type failingBodyReader struct{}

func (failingBodyReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestWriteBodyTooLarge(t *testing.T) {
	response := httptest.NewRecorder()
	writeBodyTooLarge(response)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", response.Code)
	}
}
