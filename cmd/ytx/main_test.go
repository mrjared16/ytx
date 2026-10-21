package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	ytxpkg "github.com/mrjared16/ytx/pkg"
)

func TestCategorizeError(t *testing.T) {
	login := ytxpkg.VideoAttemptError{
		Client: "WEB",
		Kind:   ytxpkg.VideoAttemptPlayability,
		Status: "LOGIN_REQUIRED",
	}
	unplayable := ytxpkg.VideoAttemptError{
		Client: "WEB",
		Kind:   ytxpkg.VideoAttemptPlayability,
		Status: "UNPLAYABLE",
		Reason: "private video",
	}
	probe := ytxpkg.VideoAttemptError{
		Client: "VISIONOS",
		Kind:   ytxpkg.VideoAttemptProbe,
		Status: "LOGIN_REQUIRED",
		Reason: "probe text must not become an auth signal",
	}
	decode := ytxpkg.VideoAttemptError{
		Client: "ANDROID",
		Kind:   ytxpkg.VideoAttemptDecode,
		Err:    errors.New("LOGIN_REQUIRED while decoding"),
	}
	rateLimited := ytxpkg.VideoAttemptError{
		Client:     "ANDROID_VR",
		Kind:       ytxpkg.VideoAttemptHTTP,
		Status:     "RATE_LIMITED",
		HTTPStatus: http.StatusTooManyRequests,
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "wrapped exhausted three login one probe",
			err: fmt.Errorf("outer: %w", &ytxpkg.VideoExhaustedError{
				Attempts: []ytxpkg.VideoAttemptError{login, login, login, probe},
			}),
			want: "AUTH_FAILED",
		},
		{
			name: "three non-login playability one login",
			err: fmt.Errorf("outer: %w", &ytxpkg.VideoExhaustedError{
				Attempts: []ytxpkg.VideoAttemptError{login, unplayable, unplayable, unplayable},
			}),
			want: "VIDEO_NOT_FOUND",
		},
		{
			name: "two login two non-login tie",
			err: fmt.Errorf("outer: %w", &ytxpkg.VideoExhaustedError{
				Attempts: []ytxpkg.VideoAttemptError{login, login, unplayable, unplayable},
			}),
			want: "API_ERROR",
		},
		{
			name: "three login with wrapped deadline cause",
			err: fmt.Errorf("outer: %w", &ytxpkg.VideoExhaustedError{
				Attempts: []ytxpkg.VideoAttemptError{login, login, login},
				Cause:    fmt.Errorf("fetch flight: %w", context.DeadlineExceeded),
			}),
			want: "API_ERROR",
		},
		{
			name: "three login with HTTP 429",
			err: fmt.Errorf("outer: %w", &ytxpkg.VideoExhaustedError{
				Attempts: []ytxpkg.VideoAttemptError{login, login, login, rateLimited},
			}),
			want: "API_ERROR",
		},
		{
			name: "probe and decode with misleading login text",
			err: fmt.Errorf("outer: %w", &ytxpkg.VideoExhaustedError{
				Attempts: []ytxpkg.VideoAttemptError{probe, decode},
			}),
			want: "API_ERROR",
		},
		{
			name: "wrapped empty exhausted",
			err:  fmt.Errorf("outer: %w", &ytxpkg.VideoExhaustedError{}),
			want: "API_ERROR",
		},
		{
			name: "ordinary legacy login required",
			err:  errors.New("video not playable: LOGIN_REQUIRED"),
			want: "AUTH_FAILED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := categorizeError(tt.err); got != tt.want {
				t.Fatalf("categorizeError() = %q, want %q", got, tt.want)
			}
		})
	}
}
