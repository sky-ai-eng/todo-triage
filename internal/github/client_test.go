package github

import (
	"errors"
	"fmt"
	"testing"
)

func TestHTTPError_Error(t *testing.T) {
	err := &HTTPError{StatusCode: 406, Body: `{"message":"diff too large"}`, msg: "GET /repos/o/r/pulls/1 returned 406: diff too large"}
	want := "GET /repos/o/r/pulls/1 returned 406: diff too large"
	if err.Error() != want {
		t.Errorf("HTTPError.Error() = %q, want %q", err.Error(), want)
	}
}

func TestIsHTTP406_True(t *testing.T) {
	err := &HTTPError{StatusCode: 406, Body: "diff too large", msg: "406"}
	if !IsHTTP406(err) {
		t.Error("IsHTTP406 should return true for a 406 HTTPError")
	}
}

func TestIsHTTP406_OtherStatusCodes(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 422, 500, 502} {
		err := &HTTPError{StatusCode: code, Body: "error", msg: fmt.Sprintf("%d", code)}
		if IsHTTP406(err) {
			t.Errorf("IsHTTP406 should return false for status %d", code)
		}
	}
}

func TestIsHTTP406_NonHTTPError(t *testing.T) {
	err := errors.New("network timeout")
	if IsHTTP406(err) {
		t.Error("IsHTTP406 should return false for a plain error")
	}
}

func TestIsHTTP406_Nil(t *testing.T) {
	if IsHTTP406(nil) {
		t.Error("IsHTTP406 should return false for nil")
	}
}

func TestIsHTTP406_Wrapped(t *testing.T) {
	inner := &HTTPError{StatusCode: 406, Body: "diff too large", msg: "406"}
	wrapped := fmt.Errorf("getDiffLines: %w", inner)
	if !IsHTTP406(wrapped) {
		t.Error("IsHTTP406 should unwrap and find the 406 error")
	}
}

func TestIsHTTP406_WrappedNon406(t *testing.T) {
	inner := &HTTPError{StatusCode: 500, Body: "internal server error", msg: "500"}
	wrapped := fmt.Errorf("request failed: %w", inner)
	if IsHTTP406(wrapped) {
		t.Error("IsHTTP406 should return false when wrapped error is not 406")
	}
}

func TestGraphQLURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "github.com",
			baseURL: "https://api.github.com",
			want:    "https://api.github.com/graphql",
		},
		{
			name:    "GHES",
			baseURL: "https://git.corp.example.com/api/v3",
			want:    "https://git.corp.example.com/api/graphql",
		},
		{
			name:    "GHES trailing slash stripped",
			baseURL: "https://github.example.com/api/v3",
			want:    "https://github.example.com/api/graphql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := graphqlURL(tt.baseURL)
			if got != tt.want {
				t.Errorf("graphqlURL(%q) = %q, want %q", tt.baseURL, got, tt.want)
			}
		})
	}
}
