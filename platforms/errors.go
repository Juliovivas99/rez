package platforms

import "errors"

// ErrRateLimited is returned when the provider signals throttling (HTTP 429/500, etc.).
var ErrRateLimited = errors.New("reservation API rate limited")

func isRateLimitStatus(code int) bool {
	return code == 429 || code == 500 || code == 403
}
