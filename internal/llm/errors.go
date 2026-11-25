package llm

import "strings"

type LLMError struct {
	StatusCode int
	Message    string
}

func (e *LLMError) Error() string { return e.Message }

// IsTransient reports whether the API error represents a retryable upstream 5xx/529 failure.
func (e *LLMError) IsTransient() bool {
	if e == nil {
		return false
	}
	switch e.StatusCode {
	case 500, 502, 503, 504, 529:
		return true
	}
	for _, code := range []string{"(500)", "(502)", "(503)", "(504)", "(529)"} {
		if strings.Contains(e.Message, code) {
			return true
		}
	}
	return false
}

type AuthenticationError struct {
	Message string
}

func (e *AuthenticationError) Error() string { return e.Message }

type RateLimitError struct {
	Message    string
	RetryAfter string
}

func (e *RateLimitError) Error() string { return e.Message }

type NetworkError struct {
	Message string
}

func (e *NetworkError) Error() string { return e.Message }

type ContextTooLongError struct {
	Message string
}

func (e *ContextTooLongError) Error() string { return e.Message }
